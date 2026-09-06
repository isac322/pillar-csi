//go:build docker_e2e

package dockere2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const operationTimeout = 6 * time.Minute

type suiteConfig struct {
	topology      string
	storageClass  string
	clientNodeA   string
	clientNodeB   string
	targetAddress string
}

func loadConfig(t *testing.T) suiteConfig {
	t.Helper()
	cfg := suiteConfig{
		topology:      requireEnv(t, "PILLAR_E2E_TOPOLOGY"),
		storageClass:  requireEnv(t, "PILLAR_E2E_STORAGE_CLASS"),
		clientNodeA:   requireEnv(t, "PILLAR_E2E_CLIENT_NODE_A"),
		clientNodeB:   requireEnv(t, "PILLAR_E2E_CLIENT_NODE_B"),
		targetAddress: requireEnv(t, "PILLAR_E2E_TARGET_ADDRESS"),
	}
	if cfg.clientNodeA == cfg.clientNodeB {
		t.Fatalf("client nodes must differ, both are %q", cfg.clientNodeA)
	}
	return cfg
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("required environment variable %s is empty", name)
	}
	return value
}

func TestDeployedTopology(t *testing.T) {
	cfg := loadConfig(t)
	for _, node := range []string{cfg.clientNodeA, cfg.clientNodeB} {
		got := kubectl(t, "get", "node", node, "-o", "jsonpath={.metadata.labels.pillar-csi\\.bhyoo\\.com/e2e-role}")
		if got != "client" {
			t.Fatalf("node %q e2e role = %q, want client", node, got)
		}
	}

	waitFor(t, "PillarAgent Ready", func() (bool, string) {
		status := kubectl(
			t, "get", "pillaragent", "pillar-e2e-agent", "-o",
			`jsonpath={.status.conditions[?(@.type=="Ready")].status}`,
		)
		return status == "True", status
	})

	if cfg.topology == "external" {
		desired := kubectl(
			t, "-n", "pillar-csi-system", "get", "daemonset", "pillar-csi-agent",
			"-o", "jsonpath={.status.desiredNumberScheduled}",
		)
		if desired != "0" {
			t.Fatalf("external topology scheduled %s in-cluster agent pods, want 0", desired)
		}
	}
}

func TestFilesystemCrossNodeReattach(t *testing.T) {
	cfg := loadConfig(t)
	ns := createNamespace(t, "fs")
	defer deleteNamespace(t, ns)

	apply(t, fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
  namespace: %s
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: %s
  resources:
    requests:
      storage: 64Mi
`, ns, cfg.storageClass))

	createFilesystemPod(t, ns, "writer", "data", cfg.clientNodeA)
	waitForPodReady(t, ns, "writer")
	assertPodNode(t, ns, "writer", cfg.clientNodeA)
	assertPVTarget(t, ns, "data", cfg.targetAddress)

	const payload = "pillar-csi-cross-node-payload"
	kubectl(
		t, "-n", ns, "exec", "writer", "--", "sh", "-c",
		fmt.Sprintf("printf '%%s' %q > /data/payload && sync", payload),
	)
	got := kubectl(t, "-n", ns, "exec", "writer", "--", "cat", "/data/payload")
	if got != payload {
		t.Fatalf("writer read %q, want %q", got, payload)
	}

	kubectl(t, "-n", ns, "delete", "pod", "writer", "--wait=true", "--timeout=3m")
	createFilesystemPod(t, ns, "reader", "data", cfg.clientNodeB)
	waitForPodReady(t, ns, "reader")
	assertPodNode(t, ns, "reader", cfg.clientNodeB)
	got = kubectl(t, "-n", ns, "exec", "reader", "--", "cat", "/data/payload")
	if got != payload {
		t.Fatalf("cross-node reader read %q, want %q", got, payload)
	}
}

func TestRawBlockIO(t *testing.T) {
	cfg := loadConfig(t)
	ns := createNamespace(t, "block")
	defer deleteNamespace(t, ns)

	apply(t, fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: raw
  namespace: %s
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Block
  storageClassName: %s
  resources:
    requests:
      storage: 64Mi
---
apiVersion: v1
kind: Pod
metadata:
  name: raw-io
  namespace: %s
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: %s
  containers:
    - name: test
      image: busybox:1.38.0
      command: ["sh", "-c", "sleep 3600"]
      volumeDevices:
        - name: data
          devicePath: /dev/pillar
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: raw
`, ns, cfg.storageClass, ns, cfg.clientNodeA))

	waitForPodReady(t, ns, "raw-io")
	assertPodNode(t, ns, "raw-io", cfg.clientNodeA)
	assertPVTarget(t, ns, "raw", cfg.targetAddress)
	kubectl(t, "-n", ns, "exec", "raw-io", "--", "sh", "-c", "test -b /dev/pillar")

	const marker = "pillar-csi-raw-block"
	kubectl(
		t, "-n", ns, "exec", "raw-io", "--", "sh", "-c",
		fmt.Sprintf("printf '%%s' %q | dd of=/dev/pillar bs=1 conv=fsync", marker),
	)
	got := kubectl(
		t, "-n", ns, "exec", "raw-io", "--", "sh", "-c",
		fmt.Sprintf("dd if=/dev/pillar bs=1 count=%d", len(marker)),
	)
	if got != marker {
		t.Fatalf("raw block read %q, want %q", got, marker)
	}
}

func TestOnlineFilesystemExpansion(t *testing.T) {
	cfg := loadConfig(t)
	ns := createNamespace(t, "expand")
	defer deleteNamespace(t, ns)

	apply(t, fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: expandable
  namespace: %s
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: %s
  resources:
    requests:
      storage: 64Mi
`, ns, cfg.storageClass))
	createFilesystemPod(t, ns, "expander", "expandable", cfg.clientNodeA)
	waitForPodReady(t, ns, "expander")
	assertPVTarget(t, ns, "expandable", cfg.targetAddress)
	kubectl(t, "-n", ns, "exec", "expander", "--", "sh", "-c", "printf expansion-data > /data/payload && sync")

	before := filesystemBytes(t, ns, "expander")
	kubectl(
		t, "-n", ns, "patch", "pvc", "expandable", "--type=merge", "-p",
		`{"spec":{"resources":{"requests":{"storage":"128Mi"}}}}`,
	)

	waitFor(t, "PVC capacity to reach 128Mi", func() (bool, string) {
		quantity := kubectl(t, "-n", ns, "get", "pvc", "expandable", "-o", "jsonpath={.status.capacity.storage}")
		return quantityBytes(quantity) >= 128*1024*1024, quantity
	})
	waitFor(t, "mounted filesystem to grow", func() (bool, string) {
		after := filesystemBytes(t, ns, "expander")
		return after > before, fmt.Sprintf("before=%d after=%d", before, after)
	})

	got := kubectl(t, "-n", ns, "exec", "expander", "--", "cat", "/data/payload")
	if got != "expansion-data" {
		t.Fatalf("payload after expansion = %q, want expansion-data", got)
	}
}

func createNamespace(t *testing.T, purpose string) string {
	t.Helper()
	name := fmt.Sprintf("pillar-%s-%d", purpose, time.Now().UnixNano())
	apply(t, fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n  labels:\n    pillar-csi.bhyoo.com/docker-e2e: \"true\"\n", name))
	return name
}

func deleteNamespace(t *testing.T, namespace string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	pvOutput, _, listErr := runKubectl(
		ctx,
		"get", "pv", "-o",
		fmt.Sprintf(`jsonpath={range .items[?(@.spec.claimRef.namespace==%q)]}{.metadata.name}{"\n"}{end}`, namespace),
	)
	if listErr != nil {
		t.Errorf("list PVs for cleanup namespace %q: %v", namespace, listErr)
	}
	pvs := strings.Fields(pvOutput)

	_, stderr, err := runKubectl(ctx, "delete", "namespace", namespace, "--wait=true", "--timeout=3m")
	if err != nil {
		t.Errorf("cleanup namespace %q: %v\n%s", namespace, err, stderr)
		return
	}
	for _, pv := range pvs {
		pvCtx, pvCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		_, pvStderr, waitErr := runKubectl(
			pvCtx, "wait", "--for=delete", "pv/"+pv, "--timeout=90s",
		)
		pvCancel()
		if waitErr != nil {
			t.Errorf("wait for PV %q deletion: %v\n%s", pv, waitErr, pvStderr)
		}
	}
}

func createFilesystemPod(t *testing.T, namespace, name, claim, node string) {
	t.Helper()
	apply(t, fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: %s
  containers:
    - name: test
      image: busybox:1.38.0
      command: ["sh", "-c", "sleep 3600"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: %s
`, name, namespace, node, claim))
}

func waitForPodReady(t *testing.T, namespace, name string) {
	t.Helper()
	kubectl(t, "-n", namespace, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=5m")
}

func assertPodNode(t *testing.T, namespace, pod, want string) {
	t.Helper()
	got := kubectl(t, "-n", namespace, "get", "pod", pod, "-o", "jsonpath={.spec.nodeName}")
	if got != want {
		t.Fatalf("pod %s/%s node = %q, want %q", namespace, pod, got, want)
	}
}

func assertPVTarget(t *testing.T, namespace, claim, wantAddress string) {
	t.Helper()
	waitFor(t, "PVC to bind", func() (bool, string) {
		phase := kubectl(t, "-n", namespace, "get", "pvc", claim, "-o", "jsonpath={.status.phase}")
		return phase == "Bound", phase
	})
	pv := kubectl(t, "-n", namespace, "get", "pvc", claim, "-o", "jsonpath={.spec.volumeName}")
	address := kubectl(t, "get", "pv", pv, "-o", "jsonpath={.spec.csi.volumeAttributes.address}")
	if address != wantAddress {
		t.Fatalf("PV %s target address = %q, want %q", pv, address, wantAddress)
	}
	nqn := kubectl(t, "get", "pv", pv, "-o", "jsonpath={.spec.csi.volumeAttributes.target_id}")
	if !strings.HasPrefix(nqn, "nqn.") {
		t.Fatalf("PV %s target_id = %q, want an NQN", pv, nqn)
	}
}

func filesystemBytes(t *testing.T, namespace, pod string) int64 {
	t.Helper()
	value := kubectl(t, "-n", namespace, "exec", pod, "--", "sh", "-c", "df -k /data | tail -1 | awk '{print $2}'")
	sizeKiB, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		t.Fatalf("parse filesystem size %q: %v", value, err)
	}
	return sizeKiB * 1024
}

func quantityBytes(value string) int64 {
	value = strings.TrimSpace(value)
	for suffix, multiplier := range map[string]int64{"Ki": 1024, "Mi": 1024 * 1024, "Gi": 1024 * 1024 * 1024} {
		number, found := strings.CutSuffix(value, suffix)
		if !found {
			continue
		}
		n, err := strconv.ParseInt(number, 10, 64)
		if err != nil {
			return 0
		}
		return n * multiplier
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func waitFor(t *testing.T, description string, condition func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(operationTimeout)
	var last string
	for time.Now().Before(deadline) {
		ok, state := condition()
		last = state
		if ok {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for %s; last state: %s", description, last)
}

func apply(t *testing.T, manifest string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl apply: %v\n%s\nmanifest:\n%s", err, output, manifest)
	}
}

func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	stdout, stderr, err := runKubectl(ctx, args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout, stderr)
	}
	return strings.TrimSpace(stdout)
}

func runKubectl(ctx context.Context, args ...string) (stdoutText, stderrText string, err error) {
	// The test owns every kubectl argument; none are derived from untrusted input.
	cmd := exec.CommandContext(ctx, "kubectl", args...) //nolint:gosec // G204: controlled test command.
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}
