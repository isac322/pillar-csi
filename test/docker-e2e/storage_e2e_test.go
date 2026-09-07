//go:build docker_e2e

package dockere2e

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	operationTimeout    = 6 * time.Minute
	nvmeACLProbeTimeout = 30 * time.Second
)

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
	target := readPVNVMeTarget(t, ns, "data", cfg.targetAddress)

	const payload = "pillar-csi-cross-node-payload"
	kubectl(
		t, "-n", ns, "exec", "writer", "--", "sh", "-c",
		fmt.Sprintf("printf '%%s' %q > /data/payload && sync", payload),
	)
	got := kubectl(t, "-n", ns, "exec", "writer", "--", "cat", "/data/payload")
	if got != payload {
		t.Fatalf("writer read %q, want %q", got, payload)
	}

	verifyNVMeCrossNodeHandoff(
		t, ns, "writer", "reader", target, cfg.clientNodeA, cfg.clientNodeB,
		func() {
			createFilesystemPod(t, ns, "reader", "data", cfg.clientNodeB)
		},
	)
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
`, ns, cfg.storageClass))
	createRawBlockPod(t, ns, "raw-writer", "raw", cfg.clientNodeA)

	waitForPodReady(t, ns, "raw-writer")
	assertPodNode(t, ns, "raw-writer", cfg.clientNodeA)
	target := readPVNVMeTarget(t, ns, "raw", cfg.targetAddress)
	kubectl(t, "-n", ns, "exec", "raw-writer", "--", "sh", "-c", "test -b /dev/pillar")

	const marker = "pillar-csi-raw-block"
	kubectl(
		t, "-n", ns, "exec", "raw-writer", "--", "sh", "-c",
		fmt.Sprintf("printf '%%s' %q | dd of=/dev/pillar bs=1 conv=fsync", marker),
	)
	verifyNVMeCrossNodeHandoff(
		t, ns, "raw-writer", "raw-reader", target, cfg.clientNodeA, cfg.clientNodeB,
		func() {
			createRawBlockPod(t, ns, "raw-reader", "raw", cfg.clientNodeB)
		},
	)
	got := kubectl(
		t, "-n", ns, "exec", "raw-reader", "--", "sh", "-c",
		fmt.Sprintf("dd if=/dev/pillar bs=1 count=%d", len(marker)),
	)
	if got != marker {
		t.Fatalf("cross-node raw block read %q, want %q", got, marker)
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
	readPVNVMeTarget(t, ns, "expandable", cfg.targetAddress)
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

func createRawBlockPod(t *testing.T, namespace, name, claim, node string) {
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
      volumeDevices:
        - name: data
          devicePath: /dev/pillar
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

type nvmeTarget struct {
	nqn      string
	address  string
	port     string
	endpoint netip.AddrPort
}

type nvmeControllerState struct {
	name      string
	transport string
	address   string
}

type nvmeNodeState struct {
	networkNamespace  string
	controllers       []nvmeControllerState
	devices           []string
	staleDevices      []string
	targetPortSockets []netip.AddrPort
	inspectionErrors  []string
}

type nvmeHostIdentity struct {
	nqn string
	id  string
}

func readPVNVMeTarget(t *testing.T, namespace, claim, wantAddress string) nvmeTarget {
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
	port := kubectl(t, "get", "pv", pv, "-o", "jsonpath={.spec.csi.volumeAttributes.port}")
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		t.Fatalf("PV %s target port = %q, want a TCP port: %v", pv, port, err)
	}
	targetAddress, err := netip.ParseAddr(address)
	if err != nil {
		t.Fatalf("PV %s target address = %q, want an IP address: %v", pv, address, err)
	}
	targetAddress = targetAddress.Unmap()
	if targetAddress.Is6() {
		targetAddress = targetAddress.WithZone("")
	}
	return nvmeTarget{
		nqn:      nqn,
		address:  address,
		port:     port,
		endpoint: netip.AddrPortFrom(targetAddress, uint16(portNumber)),
	}
}

func verifyNVMeCrossNodeHandoff(
	t *testing.T,
	namespace, writerPod, readerPod string,
	target nvmeTarget,
	clientA, clientB string,
	createReader func(),
) {
	t.Helper()

	writerState := readNVMeNodeState(t, clientA, target)
	readerState := readNVMeNodeState(t, clientB, target)
	if writerState.networkNamespace == readerState.networkNamespace {
		t.Fatalf(
			"Kind clients %q and %q share network namespace %q; cannot verify initiator isolation",
			clientA, clientB, writerState.networkNamespace,
		)
	}
	requireNVMeConnected(t, clientA, target, writerState)
	requireNVMeDisconnected(t, clientB, target, readerState)

	writerIdentity := readNVMeHostIdentity(t, clientA)
	readerIdentity := readNVMeHostIdentity(t, clientB)
	if writerIdentity.nqn == readerIdentity.nqn {
		t.Fatalf("Kind clients %q and %q share NVMe host NQN %q", clientA, clientB, writerIdentity.nqn)
	}
	if writerIdentity.id == readerIdentity.id {
		t.Fatalf("Kind clients %q and %q share NVMe host ID %q", clientA, clientB, writerIdentity.id)
	}
	requireNVMeTCPReachable(t, clientB, target)
	assertUnauthorizedNVMeConnectRejected(t, clientB, target, readerIdentity)
	waitFor(t, fmt.Sprintf("rejected NVMe connect on Kind node %q to leave no trace", clientB), func() (bool, string) {
		state := readNVMeNodeState(t, clientB, target)
		ok, reason := nvmeDisconnectedState(target, state)
		return ok, reason + "; " + state.String()
	})

	kubectl(t, "-n", namespace, "delete", "pod", writerPod, "--wait=true", "--timeout=3m")
	waitForNVMeDetached(t, clientA, target)

	createReader()
	waitForPodReady(t, namespace, readerPod)
	assertPodNode(t, namespace, readerPod, clientB)
	requireNVMeConnected(t, clientB, target, readNVMeNodeState(t, clientB, target))
	requireNVMeDisconnected(t, clientA, target, readNVMeNodeState(t, clientA, target))
}

func readNVMeNodeState(t *testing.T, node string, target nvmeTarget) nvmeNodeState {
	t.Helper()
	const inspectScript = `set -u
printf 'netns\t%s\n' "$(readlink /proc/self/ns/net)"
report_namespace_device() {
	entry=$1
	name=${entry##*/}
	if [ ! -b "/dev/${name}" ]; then
		return 0
	fi
	if ! local_dev=$(stat -c '%t:%T' "/dev/${name}" 2>/dev/null); then
		printf 'error\tstat /dev/%s\n' "${name}"
		return
	fi
	if ! sysfs_dev=$(awk -F: '{printf "%x:%x", $1, $2}' "${entry}/dev" 2>/dev/null); then
		printf 'error\tread %s/dev\n' "${entry}"
		return
	fi
	if [ "${local_dev}" = "${sysfs_dev}" ]; then
		printf 'device\t%s\n' "${name}"
	else
		printf 'stale-device\t/dev/%s local=%s sysfs=%s\n' \
			"${name}" "${local_dev}" "${sysfs_dev}"
	fi
}
for subsystem in /sys/class/nvme-subsystem/*; do
	[ -r "${subsystem}/subsysnqn" ] || continue
	if ! subsystem_nqn=$(cat "${subsystem}/subsysnqn" 2>/dev/null); then
		printf 'error\tread %s/subsysnqn\n' "${subsystem}"
		continue
	fi
	[ "${subsystem_nqn}" = "$1" ] || continue
	for entry in "${subsystem}"/nvme*; do
		[ -e "${entry}" ] || continue
		name=${entry##*/}
		case "${name#nvme}" in
			*n*)
				report_namespace_device "${entry}"
				;;
			*)
				controller="/sys/class/nvme/${name}"
				if ! transport=$(cat "${controller}/transport" 2>/dev/null); then
					printf 'error\tread %s/transport\n' "${controller}"
					continue
				fi
				if ! address=$(cat "${controller}/address" 2>/dev/null); then
					printf 'error\tread %s/address\n' "${controller}"
					continue
				fi
				printf 'controller\t%s\t%s\t%s\n' "${name}" "${transport}" "${address}"
				for namespace in "${controller}/${name}"n*; do
					[ -e "${namespace}" ] || continue
					report_namespace_device "${namespace}"
				done
				;;
		esac
	done
done`
	output := dockerExec(t, node, "sh", "-ceu", inspectScript, "inspect-nvme", target.nqn)
	state := nvmeNodeState{}
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		switch fields[0] {
		case "netns":
			if len(fields) != 2 || fields[1] == "" {
				t.Fatalf("parse NVMe state from Kind node %q: invalid netns record %q", node, line)
			}
			state.networkNamespace = fields[1]
		case "controller":
			if len(fields) != 4 {
				t.Fatalf("parse NVMe state from Kind node %q: invalid controller record %q", node, line)
			}
			state.controllers = append(state.controllers, nvmeControllerState{
				name:      fields[1],
				transport: fields[2],
				address:   fields[3],
			})
		case "device":
			if len(fields) != 2 {
				t.Fatalf("parse NVMe state from Kind node %q: invalid device record %q", node, line)
			}
			state.devices = append(state.devices, "/dev/"+fields[1])
		case "stale-device":
			if len(fields) != 2 {
				t.Fatalf("parse NVMe state from Kind node %q: invalid stale device record %q", node, line)
			}
			state.staleDevices = append(state.staleDevices, fields[1])
		case "error":
			if len(fields) != 2 {
				t.Fatalf("parse NVMe state from Kind node %q: invalid inspection error record %q", node, line)
			}
			state.inspectionErrors = append(state.inspectionErrors, fields[1])
		default:
			t.Fatalf("parse NVMe state from Kind node %q: unknown record %q", node, line)
		}
	}
	if state.networkNamespace == "" {
		t.Fatalf("NVMe state from Kind node %q did not report its network namespace", node)
	}

	const socketsScript = `set -eu
cat /proc/net/tcp
if [ -r /proc/net/tcp6 ]; then
	cat /proc/net/tcp6
fi`
	socketOutput := dockerExec(t, node, "sh", "-ceu", socketsScript)
	endpoints, err := parseEstablishedTCPEndpoints(socketOutput)
	if err != nil {
		t.Fatalf("parse established TCP sockets in Kind node %q: %v", node, err)
	}
	for _, endpoint := range endpoints {
		if endpoint.Port() == target.endpoint.Port() {
			state.targetPortSockets = append(state.targetPortSockets, endpoint)
		}
	}
	return state
}

func readNVMeHostIdentity(t *testing.T, node string) nvmeHostIdentity {
	t.Helper()
	const identityScript = `set -eu
test -c /dev/nvme-fabrics
hostnqn=$(cat /etc/nvme/hostnqn)
hostid=$(cat /etc/nvme/hostid)
test -n "${hostnqn}"
test -n "${hostid}"
printf 'hostnqn\t%s\nhostid\t%s\n' "${hostnqn}" "${hostid}"`
	output := dockerExec(t, node, "sh", "-ceu", identityScript)
	identity := nvmeHostIdentity{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 || fields[1] == "" {
			t.Fatalf("parse NVMe host identity from Kind node %q: invalid record %q", node, line)
		}
		switch fields[0] {
		case "hostnqn":
			identity.nqn = fields[1]
		case "hostid":
			identity.id = fields[1]
		default:
			t.Fatalf("parse NVMe host identity from Kind node %q: unknown record %q", node, line)
		}
	}
	if identity.nqn == "" || identity.id == "" {
		t.Fatalf("NVMe host identity from Kind node %q is incomplete: %+v", node, identity)
	}
	return identity
}

func requireNVMeTCPReachable(t *testing.T, node string, target nvmeTarget) {
	t.Helper()
	const reachedTargetMarker = "target-tcp-reachable"
	const reachabilityScript = `set -eu
timeout 5 bash -ceu ': >"/dev/tcp/$1/$2"' "nvme-tcp-reachability" "$1" "$2"
printf '%s\n' '` + reachedTargetMarker + `'`
	ctx, cancel := context.WithTimeout(context.Background(), nvmeACLProbeTimeout)
	defer cancel()
	stdout, stderr, err := runDockerExec(
		ctx,
		node,
		"bash", "-ceu", reachabilityScript, "probe-nvme-tcp",
		target.address, target.port,
	)
	if err != nil {
		if ctx.Err() != nil {
			t.Fatalf(
				"NVMe target TCP reachability probe from Kind node %q to %s timed out: %v\nstdout:\n%s\nstderr:\n%s",
				node, target.endpoint, ctx.Err(), stdout, stderr,
			)
		}
		t.Fatalf(
			"NVMe target TCP endpoint %s is not reachable from Kind node %q before ACL probe: %v\nstdout:\n%s\nstderr:\n%s",
			target.endpoint, node, err, stdout, stderr,
		)
	}
	if stdout != reachedTargetMarker {
		t.Fatalf(
			"NVMe target TCP reachability probe from Kind node %q to %s returned unexpected output %q",
			node, target.endpoint, stdout,
		)
	}
}

func assertUnauthorizedNVMeConnectRejected(
	t *testing.T,
	node string,
	target nvmeTarget,
	identity nvmeHostIdentity,
) {
	t.Helper()
	const reachedWriteMarker = "reached-nvme-fabrics-write"
	const connectScript = `set -eu
test -c /dev/nvme-fabrics
printf '%s\n' '` + reachedWriteMarker + `'
printf '%s\n' \
	"transport=tcp,traddr=$1,trsvcid=$2,nqn=$3,hostnqn=$4,hostid=$5" \
	> /dev/nvme-fabrics`
	ctx, cancel := context.WithTimeout(context.Background(), nvmeACLProbeTimeout)
	defer cancel()
	stdout, stderr, err := runDockerExec(
		ctx,
		node,
		"bash", "-ceu", connectScript, "unauthorized-nvme-connect",
		target.address, target.port, target.nqn, identity.nqn, identity.id,
	)
	if err == nil {
		t.Fatalf(
			"unauthorized NVMe connection from Kind node %q succeeded for nqn=%q address=%s port=%s hostnqn=%q hostid=%q",
			node, target.nqn, target.address, target.port, identity.nqn, identity.id,
		)
	}
	if ctx.Err() != nil {
		t.Fatalf(
			"unauthorized NVMe connection attempt from Kind node %q timed out: %v\nstdout:\n%s\nstderr:\n%s",
			node, ctx.Err(), stdout, stderr,
		)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf(
			"run unauthorized NVMe connection attempt from Kind node %q: %v\nstdout:\n%s\nstderr:\n%s",
			node, err, stdout, stderr,
		)
	}
	if stdout != reachedWriteMarker {
		t.Fatalf(
			"unauthorized NVMe connection attempt from Kind node %q failed before writing /dev/nvme-fabrics: %v\nstdout:\n%s\nstderr:\n%s",
			node, err, stdout, stderr,
		)
	}
	if !strings.Contains(stderr, "Input/output error") {
		t.Fatalf(
			"unauthorized NVMe connection from Kind node %q failed with a non-ACL error; want EIO from target rejection: %v\nstdout:\n%s\nstderr:\n%s",
			node, err, stdout, stderr,
		)
	}
}
func requireNVMeConnected(t *testing.T, node string, target nvmeTarget, state nvmeNodeState) {
	t.Helper()
	if ok, reason := nvmeConnectedState(target, state); !ok {
		t.Fatalf("Kind node %q is not connected to expected NVMe target: %s; %s", node, reason, state)
	}
}

func requireNVMeDisconnected(t *testing.T, node string, target nvmeTarget, state nvmeNodeState) {
	t.Helper()
	if ok, reason := nvmeDisconnectedState(target, state); !ok {
		t.Fatalf("Kind node %q is still connected to NVMe target: %s; %s", node, reason, state)
	}
}

func waitForNVMeDetached(t *testing.T, node string, target nvmeTarget) {
	t.Helper()
	waitFor(t, fmt.Sprintf("NVMe target %q to detach from Kind node %q", target.nqn, node), func() (bool, string) {
		state := readNVMeNodeState(t, node, target)
		ok, reason := nvmeDetachedState(target, state)
		return ok, reason + "; " + state.String()
	})
}

func nvmeConnectedState(target nvmeTarget, state nvmeNodeState) (bool, string) {
	if len(state.inspectionErrors) != 0 {
		return false, fmt.Sprintf("NVMe state inspection errors: %v", state.inspectionErrors)
	}
	if len(state.controllers) == 0 {
		return false, fmt.Sprintf("no controller reports subsystem NQN %q", target.nqn)
	}
	for _, controller := range state.controllers {
		if !nvmeControllerMatchesTarget(controller, target) {
			return false, fmt.Sprintf(
				"controller %s reports transport=%q address=%q, want tcp traddr=%s trsvcid=%s",
				controller.name, controller.transport, controller.address, target.address, target.port,
			)
		}
	}
	if len(state.devices) == 0 {
		return false, fmt.Sprintf("no local block device for subsystem NQN %q", target.nqn)
	}
	for _, endpoint := range state.targetPortSockets {
		if endpoint == target.endpoint {
			return true, ""
		}
	}
	return false, fmt.Sprintf("network namespace has no established TCP connection to %s", target.endpoint)
}

func nvmeDisconnectedState(target nvmeTarget, state nvmeNodeState) (bool, string) {
	if len(state.inspectionErrors) != 0 {
		return false, fmt.Sprintf("NVMe state inspection errors: %v", state.inspectionErrors)
	}
	for _, endpoint := range state.targetPortSockets {
		if endpoint == target.endpoint {
			return false, fmt.Sprintf("established TCP connection to %s remains", target.endpoint)
		}
	}
	return true, "target TCP connection is absent from the node network namespace"
}

func nvmeDetachedState(target nvmeTarget, state nvmeNodeState) (bool, string) {
	if ok, reason := nvmeDisconnectedState(target, state); !ok {
		return false, reason
	}
	if len(state.controllers) != 0 {
		return false, fmt.Sprintf("subsystem controllers remain after detach: %v", state.controllers)
	}
	return true, "controller and target TCP connection are absent"
}

func nvmeControllerMatchesTarget(controller nvmeControllerState, target nvmeTarget) bool {
	if controller.transport != "tcp" {
		return false
	}
	var address, port string
	for field := range strings.SplitSeq(controller.address, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(field), "=")
		if !found {
			continue
		}
		switch key {
		case "traddr":
			address = value
		case "trsvcid":
			port = value
		}
	}
	return address == target.address && port == target.port
}

func parseEstablishedTCPEndpoints(output string) ([]netip.AddrPort, error) {
	var endpoints []netip.AddrPort
	for lineNumber, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "sl" {
			continue
		}
		if len(fields) < 4 {
			return nil, fmt.Errorf("line %d has %d fields: %q", lineNumber+1, len(fields), line)
		}
		if fields[3] != "01" {
			continue
		}
		addressHex, portHex, found := strings.Cut(fields[2], ":")
		if !found {
			return nil, fmt.Errorf("line %d has invalid remote endpoint %q", lineNumber+1, fields[2])
		}
		address, err := parseProcNetAddress(addressHex)
		if err != nil {
			return nil, fmt.Errorf("line %d remote address %q: %w", lineNumber+1, addressHex, err)
		}
		port, err := strconv.ParseUint(portHex, 16, 16)
		if err != nil {
			return nil, fmt.Errorf("line %d remote port %q: %w", lineNumber+1, portHex, err)
		}
		endpoints = append(endpoints, netip.AddrPortFrom(address.Unmap(), uint16(port)))
	}
	return endpoints, nil
}

func parseProcNetAddress(encoded string) (netip.Addr, error) {
	raw, err := hex.DecodeString(encoded)
	if err != nil {
		return netip.Addr{}, err
	}
	switch len(raw) {
	case 4:
		raw[0], raw[3] = raw[3], raw[0]
		raw[1], raw[2] = raw[2], raw[1]
		var address [4]byte
		copy(address[:], raw)
		return netip.AddrFrom4(address), nil
	case 16:
		for offset := 0; offset < len(raw); offset += 4 {
			raw[offset], raw[offset+3] = raw[offset+3], raw[offset]
			raw[offset+1], raw[offset+2] = raw[offset+2], raw[offset+1]
		}
		var address [16]byte
		copy(address[:], raw)
		return netip.AddrFrom16(address), nil
	default:
		return netip.Addr{}, fmt.Errorf("decoded address has %d bytes, want 4 or 16", len(raw))
	}
}

func (s nvmeNodeState) String() string {
	return fmt.Sprintf(
		"netns=%q controllers=%v devices=%v stale-devices=%v target-port-sockets=%v inspection-errors=%v",
		s.networkNamespace, s.controllers, s.devices, s.staleDevices, s.targetPortSockets, s.inspectionErrors,
	)
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

func dockerExec(t *testing.T, node string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	stdout, stderr, err := runDockerExec(ctx, node, args...)
	if err != nil {
		t.Fatalf(
			"docker exec %s %s: %v\nstdout:\n%s\nstderr:\n%s",
			node, strings.Join(args, " "), err, stdout, stderr,
		)
	}
	return strings.TrimSpace(stdout)
}

func runDockerExec(
	ctx context.Context,
	node string,
	args ...string,
) (stdoutText, stderrText string, err error) {
	commandArgs := make([]string, 0, len(args)+2)
	commandArgs = append(commandArgs, "exec", node)
	commandArgs = append(commandArgs, args...)
	// The test owns the Kind node name and every command argument.
	cmd := exec.CommandContext(ctx, "docker", commandArgs...) //nolint:gosec // G204: controlled test command.
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}
