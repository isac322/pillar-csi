//go:build e2e && e2e_helm

package e2e

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	pillarv1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

// The native fixture exclusively owns source creation, quotas and destruction.
// This layer owns consumers and observations of the installed network surface.
type filesystemNetworkFixture struct {
	*FilesystemAdoptionFixture
	Clients      []string
	Pods         []string
	PodUIDs      []string
	Address      string
	Target       string
	Proxy        string
	Handle       string
	Dataset      string
	StorageClass string
}

// newFilesystemNetworkFixture adopts the source through the owned-host NFS
// class with ReadWriteMany. A localAttach class is local-only and rejects
// multi-node capabilities, so storage-node direct binds are exercised through
// this same network class: the controller publishes the storage NodeRef locally.
func newFilesystemNetworkFixture(ctx context.Context, tc, kind string) *filesystemNetworkFixture {
	f := NewFilesystemAdoptionFixture(tc)
	var n *filesystemNetworkFixture
	// Registered before the Cleanup DeferCleanup so it runs after it (LIFO): if
	// the spec or generic PV cleanup failed, capture the actual teardown state
	// (PVS, VolumeAttachments, node-plugin/controller logs) instead of letting a
	// stuck drain surface as a bare PV finalizer timeout.
	DeferCleanup(func() {
		if n == nil || !CurrentSpecReport().Failed() {
			return
		}
		fmt.Fprintln(GinkgoWriter, n.unpublishDiagnostics(append(append([]string{}, n.Clients...), n.StorageNode)...)())
	})
	DeferCleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		Expect(f.Cleanup(cleanupCtx)).To(Succeed())
	})
	if kind == "directory" {
		Expect(f.PrepareDirectory(ctx, "xfs", 64*1024*1024)).To(Succeed())
	} else {
		Expect(f.PrepareZFS(ctx, 64*1024*1024)).To(Succeed())
	}
	Expect(f.ApplyObjects(ctx, false)).To(Succeed())
	annotation := pillarv1.AnnotationImportZFSDataset
	if kind == "directory" {
		annotation = pillarv1.AnnotationImportDirectory
	}
	Expect(f.AdoptPVC(ctx, annotation, "ReadWriteMany", 64*1024*1024)).To(Succeed())
	n = &filesystemNetworkFixture{FilesystemAdoptionFixture: f}
	if kind != "directory" {
		n.Dataset = f.CanonicalSource
	}
	waitOutput, waitErr := n.Kubectl(ctx, "", "-n", n.Namespace, "wait", "--for=jsonpath={.status.phase}=Bound", "pvc/"+n.PVCName, "--timeout=3m")
	if waitErr != nil {
		diagnosticCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var diagnostics strings.Builder
		capture := func(args ...string) (string, error) {
			output, diagnosticErr := n.Kubectl(diagnosticCtx, "", args...)
			diagnosticOutput := output
			const maxDiagnosticBytes = 32 * 1024
			if len(diagnosticOutput) > maxDiagnosticBytes {
				diagnosticOutput = "[earlier output truncated]\n" + diagnosticOutput[len(diagnosticOutput)-maxDiagnosticBytes:]
			}
			fmt.Fprintf(&diagnostics, "\n\nkubectl %s:\n%s", strings.Join(args, " "), diagnosticOutput)
			if diagnosticErr != nil {
				fmt.Fprintf(&diagnostics, "\nDiagnostic command failed: %v", diagnosticErr)
			}
			return output, diagnosticErr
		}
		capture("-n", n.Namespace, "get", "pvc", n.PVCName, "-o", "yaml")
		capture("-n", n.Namespace, "describe", "pvc", n.PVCName)
		capture("-n", n.Namespace, "get", "events", "--field-selector=involvedObject.kind=PersistentVolumeClaim,involvedObject.name="+n.PVCName, "-o", "wide")
		volumeName, volumeErr := capture("-n", n.Namespace, "get", "pvc", n.PVCName, "-o", "jsonpath={.spec.volumeName}")
		if volumeName = strings.TrimSpace(volumeName); volumeErr == nil && volumeName != "" {
			capture("get", "pillarvolumestate", volumeName, "-o", "yaml")
			capture("get", "pv", volumeName, "-o", "yaml")
		}
		Fail(fmt.Sprintf("PVC %s/%s did not become Bound: %v\nWait output:\n%s%s", n.Namespace, n.PVCName, waitErr, waitOutput, diagnostics.String()))
	}
	n.PVName = n.Must(ctx, "-n", n.Namespace, "get", "pvc", n.PVCName, "-o", "jsonpath={.spec.volumeName}")
	n.StorageClass = n.Must(ctx, "-n", n.Namespace, "get", "pvc", n.PVCName, "-o", "jsonpath={.spec.storageClassName}")
	n.Must(ctx, "patch", "pv", n.PVName, "--type=merge", "-p", `{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}`)
	nodes := strings.Fields(f.Must(ctx, "get", "nodes", "-o", "jsonpath={.items[*].metadata.name}"))
	for _, node := range nodes {
		if node != f.StorageNode {
			n.Clients = append(n.Clients, node)
		}
	}
	Expect(n.Clients).To(HaveLen(2), "two different remote client nodes and a distinct storage node are required")
	n.Address = f.Must(ctx, "get", "pv", f.PVName, "-o", "jsonpath={.spec.csi.volumeAttributes.address}")
	n.Target = f.Must(ctx, "get", "pv", f.PVName, "-o", "jsonpath={.spec.csi.volumeAttributes.target_id}")
	n.Handle = f.Must(ctx, "get", "pv", f.PVName, "-o", "jsonpath={.spec.csi.volumeHandle}")
	Expect(f.Must(ctx, "get", "pv", f.PVName, "-o", "jsonpath={.spec.csi.driver}")).To(Equal(pillarv1.FileCSIDriver))
	Expect(n.Address).To(Equal(f.Must(ctx, "get", "node", f.StorageNode, "-o", `jsonpath={.status.addresses[?(@.type=="InternalIP")].address}`)))
	Expect(n.Target).To(HavePrefix("/"))
	state := n.state(ctx)
	Expect(state.Spec.FilesystemAdoption).NotTo(BeNil())
	Expect(state.Spec.FilesystemAdoption.CanonicalSource).To(Equal(f.CanonicalSource))
	Expect(state.Spec.FilesystemAdoption.ResourceID).To(Equal(f.NativeID))
	return n
}

func (n *filesystemNetworkFixture) state(ctx context.Context) pillarv1.PillarVolumeState {
	var state pillarv1.PillarVolumeState
	Expect(json.Unmarshal([]byte(n.Must(ctx, "get", "pillarvolumestate", n.PVName, "-o", "json")), &state)).To(Succeed())
	return state
}

func (n *filesystemNetworkFixture) apply(ctx context.Context, manifest string) {
	_, err := n.Kubectl(ctx, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
}

func (n *filesystemNetworkFixture) consumer(ctx context.Context, name, node string, uid, gid int64) {
	var pod map[string]any
	Expect(yaml.Unmarshal([]byte(n.PodManifest(name, n.PVCName, node, false, uid, gid)), &pod)).To(Succeed())
	spec := pod["spec"].(map[string]any)
	if node == n.StorageNode {
		spec["tolerations"] = []any{map[string]any{"operator": "Exists"}}
	}
	container := spec["containers"].([]any)[0].(map[string]any)
	// Application image supplies fcntl/fsync; no host packages or mock locking.
	container["image"] = "python:3.12-alpine"
	container["command"] = []string{"python3", "-c", "import time; time.sleep(3600)"}
	data, err := json.Marshal(pod)
	Expect(err).NotTo(HaveOccurred())
	n.apply(ctx, string(data))
	waitOutput, waitErr := n.Kubectl(ctx, "", "-n", n.Namespace, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=3m")
	if waitErr != nil {
		diagnosticCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		var diagnostics strings.Builder
		for _, args := range [][]string{
			{"get", "pod", name, "-o", "yaml"},
			{"describe", "pod", name},
			{"get", "events", "--field-selector=involvedObject.kind=Pod,involvedObject.name=" + name, "-o", "wide"},
		} {
			output, diagnosticErr := n.Kubectl(diagnosticCtx, "", append([]string{"-n", n.Namespace}, args...)...)
			const maxDiagnosticBytes = 32 * 1024
			if len(output) > maxDiagnosticBytes {
				output = "[earlier output truncated]\n" + output[len(output)-maxDiagnosticBytes:]
			}
			fmt.Fprintf(&diagnostics, "\n\nkubectl -n %s %s:\n%s", n.Namespace, strings.Join(args, " "), output)
			if diagnosticErr != nil {
				fmt.Fprintf(&diagnostics, "\nDiagnostic command failed: %v", diagnosticErr)
			}
		}
		const maxAgentDiagnosticBytes = 32 * 1024
		agentNamespace := resolveHelmNamespace()
		agentSelector := "app.kubernetes.io/component=agent"
		agentArgs := []string{"-n", agentNamespace, "get", "pods", "-l", agentSelector, "--field-selector", "spec.nodeName=" + n.StorageNode, "-o", "jsonpath={.items[0].metadata.name}"}
		agentPodName, agentErr := n.Kubectl(diagnosticCtx, "", agentArgs...)
		agentPodOutput := agentPodName
		if len(agentPodOutput) > maxAgentDiagnosticBytes {
			agentPodOutput = "[earlier output truncated]\n" + agentPodOutput[len(agentPodOutput)-maxAgentDiagnosticBytes:]
		}
		fmt.Fprintf(&diagnostics, "\n\nkubectl -n %s %s:\n%s", agentNamespace, strings.Join(agentArgs[2:], " "), agentPodOutput)
		if agentErr != nil {
			fmt.Fprintf(&diagnostics, "\nDiagnostic command failed: %v", agentErr)
		}
		if agentErr == nil && agentPodName != "" {
			execArgs := []string{"-n", agentNamespace, "exec", agentPodName, "-c", "agent", "--", "/bin/busybox", "sh", "-ceu", "cat /var/lib/nfs/etab; exportfs -v"}
			agentOutput, execErr := n.Kubectl(diagnosticCtx, "", execArgs...)
			if len(agentOutput) > maxAgentDiagnosticBytes {
				agentOutput = "[earlier output truncated]\n" + agentOutput[len(agentOutput)-maxAgentDiagnosticBytes:]
			}
			fmt.Fprintf(&diagnostics, "\n\nkubectl -n %s %s:\n%s", agentNamespace, strings.Join(execArgs[2:], " "), agentOutput)
			if execErr != nil {
				fmt.Fprintf(&diagnostics, "\nDiagnostic command failed: %v", execErr)
			}
		}
		Fail(fmt.Sprintf("Pod %s/%s did not become Ready: %v\nWait output:\n%s%s", n.Namespace, name, waitErr, waitOutput, diagnostics.String()))
	}
	Expect(n.Must(ctx, "-n", n.Namespace, "get", "pod", name, "-o", "jsonpath={.spec.nodeName}")).To(Equal(node))
	n.Pods = append(n.Pods, name)
	n.PodUIDs = append(n.PodUIDs, n.Must(ctx, "-n", n.Namespace, "get", "pod", name, "-o", "jsonpath={.metadata.uid}"))
}

func (n *filesystemNetworkFixture) ownerConsumers(ctx context.Context) {
	for i, node := range n.Clients {
		n.consumer(ctx, fmt.Sprintf("writer-%d", i), node, n.SourceUID, n.SourceGID)
	}
}

func (n *filesystemNetworkFixture) python(ctx context.Context, pod, script string) (string, error) {
	return n.Kubectl(ctx, "", "-n", n.Namespace, "exec", pod, "--", "python3", "-c", script)
}

func (n *filesystemNetworkFixture) mustPython(ctx context.Context, pod, script string) string {
	out, err := n.python(ctx, pod, script)
	Expect(err).NotTo(HaveOccurred(), "%s: %s", pod, out)
	return out
}

func (n *filesystemNetworkFixture) exchange(ctx context.Context) {
	for i := range n.Clients {
		pod := fmt.Sprintf("writer-%d", i)
		name := fmt.Sprintf("writer-%d.bin", i)
		payload := n.Namespace + "/" + n.Clients[i] + "/distinct-fsynced-payload"
		n.mustPython(ctx, pod, fmt.Sprintf("import os; p=%q; f=open('/data/'+%q,'wb'); f.write(p.encode()); f.flush(); os.fsync(f.fileno()); f.close()", payload, name))
		expected := sha256.Sum256([]byte(payload))
		other := fmt.Sprintf("writer-%d", 1-i)
		Expect(n.mustPython(ctx, other, fmt.Sprintf("import hashlib; print(hashlib.sha256(open('/data/'+%q,'rb').read()).hexdigest())", name))).To(Equal(hex.EncodeToString(expected[:])))
		outside, err := n.HostExec(ctx, "sha256sum", filepath.Join(n.MountPoint, name))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Fields(outside)[0]).To(Equal(hex.EncodeToString(expected[:])))
	}
}

func (n *filesystemNetworkFixture) verifyExchange(ctx context.Context) {
	for i := range n.Clients {
		expected := sha256.Sum256([]byte(n.Namespace + "/" + n.Clients[i] + "/distinct-fsynced-payload"))
		name := fmt.Sprintf("writer-%d.bin", i)
		other := fmt.Sprintf("writer-%d", 1-i)
		Expect(n.mustPython(ctx, other, fmt.Sprintf("import hashlib; print(hashlib.sha256(open('/data/'+%q,'rb').read()).hexdigest())", name))).To(Equal(hex.EncodeToString(expected[:])))
	}
}

func (n *filesystemNetworkFixture) observeRemoteMounts(ctx context.Context) {
	for i, node := range n.Clients {
		mount := n.mustPython(ctx, fmt.Sprintf("writer-%d", i), `print(next(line.strip() for line in open('/proc/mounts') if line.split()[1] == '/data'))`)
		fields := strings.Fields(mount)
		Expect(fields[0]).To(Equal(n.Address + ":" + n.Target))
		Expect(fields[2]).To(Equal("nfs4"))
		Expect(strings.Split(fields[3], ",")).To(ContainElement("addr=" + n.Address))
		Expect(strings.Split(fields[3], ",")).To(ContainElement("proto=tcp"))
		AddReportEntry("remote kernel mount "+node, mount)
	}
}

func (n *filesystemNetworkFixture) observeProxy(ctx context.Context) {
	policies := nfsAgentExec(ctx, "exportfs", "-v")
	// Resolve the exact physical export by its ACL and file content, not a path
	// naming convention. The mount must be an owned bind under ExportRoot.
	candidate := ""
	for _, line := range strings.Split(policies, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.HasPrefix(fields[0], "/var/lib/pillar-csi/agent/datasets/") {
			p := fields[0]
			out, err := n.HostExec(ctx, "sh", "-ceu", `[ -f "$1/writer-0.bin" ] && sha256sum "$1/writer-0.bin"`, "probe", p)
			if err == nil && strings.Fields(out)[0] == n.mustPython(ctx, "writer-0", `import hashlib; print(hashlib.sha256(open('/data/writer-0.bin','rb').read()).hexdigest())`) {
				candidate = p
				break
			}
		}
	}
	Expect(candidate).NotTo(BeEmpty(), "owned proxy export must physically reference the adopted source")
	Expect(candidate).NotTo(Equal(n.MountPoint), "the source must never be directly exported")
	n.Proxy = candidate
	mount, err := n.HostExec(ctx, "findmnt", "-n", "-M", candidate, "-o", "FSTYPE,SOURCE,FSROOT")
	Expect(err).NotTo(HaveOccurred())
	Expect(mount).NotTo(BeEmpty())
	proxyMount, err := n.HostExec(ctx, "findmnt", "-n", "-M", candidate, "-o", "ID")
	Expect(err).NotTo(HaveOccurred())
	sourceMount, err := n.HostExec(ctx, "findmnt", "-n", "-T", n.MountPoint, "-o", "ID")
	Expect(err).NotTo(HaveOccurred())
	Expect(proxyMount).NotTo(Equal(sourceMount), "the export must own a separate bind mount, not a source-path alias")
	for _, node := range n.Clients {
		ip := n.Must(ctx, "get", "node", node, "-o", `jsonpath={.status.addresses[?(@.type=="InternalIP")].address}`)
		policy := nfsExportPolicies(policies, candidate)
		Expect(policy).To(HaveKey(ip))
		Expect(policy[ip]).To(ContainElement("root_squash"))
	}
	// stat's device/inode pair proves the proxy binds the same native object.
	proxy, err := n.HostExec(ctx, "stat", "-c", "%d:%i:%u:%g:%a", candidate)
	Expect(err).NotTo(HaveOccurred())
	source, err := n.HostExec(ctx, "stat", "-c", "%d:%i:%u:%g:%a", n.MountPoint)
	Expect(err).NotTo(HaveOccurred())
	Expect(proxy).To(Equal(source))
	Expect(strings.Join(strings.Split(source, ":")[2:], ":")).To(Equal(fmt.Sprintf("%d:%d:%s", n.SourceUID, n.SourceGID, n.SourceMode)))
	if n.Dataset != "" {
		guid, err := n.HostExec(ctx, "zfs", "get", "-Hp", "-o", "value", "guid", n.Dataset)
		Expect(err).NotTo(HaveOccurred())
		Expect(guid).To(Equal(n.NativeID))
		point, err := n.HostExec(ctx, "zfs", "get", "-Hp", "-o", "value", "mountpoint", n.Dataset)
		Expect(err).NotTo(HaveOccurred())
		Expect(point).To(Equal(n.MountPoint))
	}
	AddReportEntry("physical owned source proxy", mount)
}

func (n *filesystemNetworkFixture) restart(ctx context.Context, component, node string) {
	pod := nfsComponentPod(ctx, component, node)
	uid := n.Must(ctx, "-n", resolveHelmNamespace(), "get", "pod", pod, "-o", "jsonpath={.metadata.uid}")
	n.Must(ctx, "-n", resolveHelmNamespace(), "delete", "pod", pod, "--wait=true", "--timeout=90s")
	Eventually(func() (string, error) {
		out, err := n.Kubectl(ctx, "", "-n", resolveHelmNamespace(), "get", "pods", "-l", "app.kubernetes.io/component="+component, "--field-selector=spec.nodeName="+node, "-o", "jsonpath={.items[0].metadata.uid}")
		return out, err
	}, 3*time.Minute, 2*time.Second).Should(And(Not(BeEmpty()), Not(Equal(uid))))
	n.Must(ctx, "-n", resolveHelmNamespace(), "rollout", "status", "daemonset/pillar-csi-"+component, "--timeout=3m")
}

// stageStateFile is the real stage record path for this volume.
func (n *filesystemNetworkFixture) stageStateFile() string {
	return fileNodeStageStateFile(n.Handle)
}

func (n *filesystemNetworkFixture) unpublishAll(ctx context.Context) {
	// Every live publication must hold a real stage record on its node. Witnessing
	// existence before deletion keeps the drain assertions below from passing
	// vacuously on a wrong path; publications already drained are not re-checked.
	pubs := n.state(ctx).Status.PublishedNodes
	Expect(pubs).NotTo(BeEmpty(), "unpublishAll requires live publications to witness; delete pods through this helper before the controller drains them")
	for _, pub := range pubs {
		_, err := n.NodeExec(ctx, pub.NodeID, "test", "-e", n.stageStateFile())
		Expect(err).NotTo(HaveOccurred(), "%s must persist %s while published", pub.NodeID, n.stageStateFile())
	}
	for _, pod := range n.Pods {
		n.Must(ctx, "-n", n.Namespace, "delete", "pod", pod, "--ignore-not-found", "--wait=true", "--timeout=3m")
	}
	Eventually(func() ([]pillarv1.VolumePublication, error) {
		out, err := n.Kubectl(ctx, "", "get", "pillarvolumestate", n.PVName, "--ignore-not-found", "-o", "json")
		if err != nil || out == "" {
			return nil, err
		}
		var state pillarv1.PillarVolumeState
		if err := json.Unmarshal([]byte(out), &state); err != nil {
			return nil, err
		}
		return state.Status.PublishedNodes, nil
	}, 90*time.Second, 2*time.Second).Should(BeEmpty(), n.unpublishDiagnostics())
	for _, node := range append(append([]string{}, n.Clients...), n.StorageNode) {
		stage := n.stageStateFile()
		Eventually(func() error { _, err := n.NodeExec(ctx, node, "test", "!", "-e", stage); return err }, 90*time.Second, 2*time.Second).Should(Succeed(), n.unpublishDiagnostics(node))
		mounts, err := n.NodeExec(ctx, node, "cat", "/proc/1/mountinfo")
		Expect(err).NotTo(HaveOccurred())
		Expect(mounts).NotTo(ContainSubstring(n.Address + ":" + n.Target))
		for _, uid := range n.PodUIDs {
			Expect(mounts).NotTo(ContainSubstring("/var/lib/kubelet/pods/" + uid + "/"))
		}
	}
}

// unpublishDiagnostics is a lazy Eventually description evaluated only when a
// drain barrier fails, and is also reused by the deferred failure capture in
// newFilesystemNetworkFixture. It delegates to the common bounded teardown
// collector with this fixture's real volume handle and export target.
func (n *filesystemNetworkFixture) unpublishDiagnostics(nodes ...string) func() string {
	return n.teardownDiagnostics(n.Handle, n.Target, nodes...)
}

func (n *filesystemNetworkFixture) assertWithdrawn(ctx context.Context) {
	Expect(nfsExportPolicies(nfsAgentExec(ctx, "exportfs", "-v"), n.Proxy)).To(BeEmpty())
	_, err := n.HostExec(ctx, "findmnt", "-n", "-M", n.Proxy)
	Expect(err).To(HaveOccurred())
	mounts, err := n.HostExec(ctx, "cat", "/proc/1/mountinfo")
	Expect(err).NotTo(HaveOccurred())
	Expect(mounts).NotTo(ContainSubstring(" "+n.Proxy), "no owned bind remains, including a deleted-target mount")
}

func (n *filesystemNetworkFixture) denyUnpublishedClient(ctx context.Context) {
	pod := nfsComponentPod(ctx, "node", n.StorageNode)
	exec := func(commandCtx context.Context, args ...string) (string, error) {
		return n.Kubectl(commandCtx, "", append([]string{"-n", resolveHelmNamespace(), "exec", pod, "-c", "file-node", "--"}, args...)...)
	}
	_, err := exec(ctx, "/bin/busybox", "nc", "-z", "-w", "3", n.Address, "2049")
	Expect(err).NotTo(HaveOccurred(), "listener reachability must precede access denial")
	path := "/tmp/" + n.Namespace + "-unauthorized"
	_, err = exec(ctx, "/bin/busybox", "mkdir", "-p", path)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = exec(cleanupCtx, "umount", path)
		_, err := exec(cleanupCtx, "/bin/busybox", "rmdir", path)
		Expect(err).NotTo(HaveOccurred())
	})
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err = exec(probeCtx, "mount.nfs", "-o", "vers=4.2,proto=tcp,hard,retry=0", n.Address+":"+n.Target, path)
	Expect(probeCtx.Err()).NotTo(HaveOccurred(), "timeout is not an authorization denial")
	Expect(err).To(HaveOccurred(), "unpublished client must not mount the source, regardless of root DAC")
	Expect(strings.ToLower(err.Error())).To(Or(ContainSubstring("access denied"), ContainSubstring("permission denied"), ContainSubstring("operation not permitted")))
}

func (n *filesystemNetworkFixture) agentPortForward(ctx context.Context, pod string) (string, context.CancelFunc, error) {
	pfCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(pfCtx, "kubectl", "--kubeconfig="+os.Getenv("KUBECONFIG"), "port-forward", "--address=127.0.0.1", "--namespace", resolveHelmNamespace(), "pod/"+pod, ":9500") //nolint:gosec
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, err
	}
	stop := func() { cancel(); _ = cmd.Wait() }
	type result struct {
		address string
		err     error
	}
	ready := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) == 5 && fields[0] == "Forwarding" && fields[1] == "from" && strings.HasPrefix(fields[2], "127.0.0.1:") {
				ready <- result{address: fields[2]}
				_, _ = io.Copy(io.Discard, stdout)
				return
			}
		}
		ready <- result{err: fmt.Errorf("agent port-forward exited before binding a listener: %v", scanner.Err())}
	}()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case result := <-ready:
		if result.err != nil {
			stop()
			return "", nil, result.err
		}
		return result.address, stop, nil
	case <-ctx.Done():
		stop()
		return "", nil, ctx.Err()
	case <-timer.C:
		stop()
		return "", nil, fmt.Errorf("agent port-forward did not bind a listener")
	}
}

func (n *filesystemNetworkFixture) withAgent(ctx context.Context, operation func(agentv1.AgentServiceClient)) {
	pod := nfsComponentPod(ctx, "agent", n.StorageNode)
	address, stop, err := n.agentPortForward(ctx, pod)
	Expect(err).NotTo(HaveOccurred())
	defer stop()
	client, conn, err := e33AgentGRPCClient(ctx, address)
	Expect(err).NotTo(HaveOccurred())
	defer func() { Expect(conn.Close()).To(Succeed()) }()
	operation(client)
}
