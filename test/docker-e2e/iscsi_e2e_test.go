//go:build docker_e2e

package dockere2e

// iscsi_e2e_test.go — the iSCSI data path on real kernels: LIO targets the
// agent builds in configfs, sessions the in-process pillar-node initiator
// hands to iscsi_tcp over NETLINK_ISCSI, and the resulting /dev/sd* devices.
//
// Kind nodes share the host kernel, so the iSCSI sysfs tree and the LIO
// configfs tree are global.  Sessions are attributed to a node by their
// initiatorname (each node persists its own IQN in its /etc/iscsi) and by the
// established TCP connection in the node's own network namespace.

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	iscsiOwnedIQNPrefix = "iqn.2026-01.com.bhyoo.pillar-csi:"
	lioISCSIRoot        = "/sys/kernel/config/target/iscsi"
	lioIBlockHBA        = "/sys/kernel/config/target/core/iblock_3260"
	lioTPG              = "tpgt_1"
	iscsiLoginTimeout   = 30 * time.Second

	// Login response status (RFC 7143 §11.13.5): class 2 "initiator error",
	// detail 2 "target forbidden" — what LIO answers an initiator without a
	// node ACL on a TPG that enforces ACLs.
	iscsiStatusClassInitiatorError = 0x02
	iscsiStatusDetailForbidden     = 0x02
)

type iscsiConfig struct {
	suiteConfig
	storageClass    string
	xfsStorageClass string
	protocol        string
}

func loadISCSIConfig(t *testing.T) iscsiConfig {
	t.Helper()
	return iscsiConfig{
		suiteConfig:     loadConfig(t),
		storageClass:    requireEnv(t, "PILLAR_E2E_ISCSI_STORAGE_CLASS"),
		xfsStorageClass: requireEnv(t, "PILLAR_E2E_ISCSI_XFS_STORAGE_CLASS"),
		protocol:        requireEnv(t, "PILLAR_E2E_ISCSI_PROTOCOL"),
	}
}

type iscsiTarget struct {
	iqn      string
	address  string
	port     string
	endpoint netip.AddrPort
}

// TestISCSIFilesystemPodRestart writes through an ext4 and an xfs iSCSI
// volume, restarts the pod on the same node and reads the data back.  The
// xfs volume is larger because mkfs.xfs (xfsprogs >= 5.19) refuses
// filesystems smaller than 300 MB.
func TestISCSIFilesystemPodRestart(t *testing.T) {
	cfg := loadISCSIConfig(t)
	for _, tc := range []struct {
		fsType       string
		storageClass string
		size         string
	}{
		{fsType: "ext4", storageClass: cfg.storageClass, size: iscsiVolumeSize},
		{fsType: "xfs", storageClass: cfg.xfsStorageClass, size: "320Mi"},
	} {
		t.Run(tc.fsType, func(t *testing.T) {
			ns := createNamespace(t, "iscsi-"+tc.fsType)
			defer deleteNamespace(t, ns)
			createISCSIPVC(t, ns, "data", tc.storageClass, "Filesystem", tc.size)

			createFilesystemPod(t, ns, "writer", "data", cfg.clientNodeA)
			waitForPodReady(t, ns, "writer")
			assertPodNode(t, ns, "writer", cfg.clientNodeA)
			target := readPVISCSITarget(t, ns, "data", cfg.targetAddress)
			t.Cleanup(func() { waitForLIOTargetRemoved(t, target) })
			iqnA := readISCSIInitiatorIQN(t, cfg.clientNodeA)
			requirePodUsesISCSIDevice(t, ns, "writer", cfg.clientNodeA, target, iqnA, false)
			if got := podMountFSType(t, ns, "writer"); got != tc.fsType {
				t.Fatalf("/data filesystem = %q, want %q", got, tc.fsType)
			}

			payload := "pillar-csi-iscsi-" + tc.fsType
			kubectl(t, "-n", ns, "exec", "writer", "--", "sh", "-c",
				fmt.Sprintf("printf '%%s' %q > /data/payload && sync", payload))

			deletePod(t, ns, "writer")
			waitForISCSIDetached(t, cfg.clientNodeA, target, iqnA)

			createFilesystemPod(t, ns, "restarted", "data", cfg.clientNodeA)
			waitForPodReady(t, ns, "restarted")
			requirePodUsesISCSIDevice(t, ns, "restarted", cfg.clientNodeA, target, iqnA, false)
			if got := kubectl(t, "-n", ns, "exec", "restarted", "--", "cat", "/data/payload"); got != payload {
				t.Fatalf("restarted pod read %q, want %q", got, payload)
			}
		})
	}
}

// TestISCSIFilesystemCrossNodeReattach moves a filesystem volume from client
// node A to client node B.  Each client reaches the storage node's portal
// over its own session, and node B only after node A released the volume.
func TestISCSIFilesystemCrossNodeReattach(t *testing.T) {
	cfg := loadISCSIConfig(t)
	ns := createNamespace(t, "iscsi-fs")
	defer deleteNamespace(t, ns)
	createISCSIPVC(t, ns, "data", cfg.storageClass, "Filesystem", iscsiVolumeSize)

	createFilesystemPod(t, ns, "writer", "data", cfg.clientNodeA)
	waitForPodReady(t, ns, "writer")
	assertPodNode(t, ns, "writer", cfg.clientNodeA)
	target := readPVISCSITarget(t, ns, "data", cfg.targetAddress)
	t.Cleanup(func() { waitForLIOTargetRemoved(t, target) })
	requirePodUsesISCSIDevice(t, ns, "writer", cfg.clientNodeA, target, readISCSIInitiatorIQN(t, cfg.clientNodeA), false)

	const payload = "pillar-csi-iscsi-cross-node"
	kubectl(t, "-n", ns, "exec", "writer", "--", "sh", "-c",
		fmt.Sprintf("printf '%%s' %q > /data/payload && sync", payload))

	verifyISCSICrossNodeHandoff(t, ns, "writer", "reader", target, cfg.clientNodeA, cfg.clientNodeB, func() {
		createFilesystemPod(t, ns, "reader", "data", cfg.clientNodeB)
	})
	requirePodUsesISCSIDevice(t, ns, "reader", cfg.clientNodeB, target, readISCSIInitiatorIQN(t, cfg.clientNodeB), false)
	if got := kubectl(t, "-n", ns, "exec", "reader", "--", "cat", "/data/payload"); got != payload {
		t.Fatalf("cross-node reader read %q, want %q", got, payload)
	}
}

// TestISCSIRawBlockCrossNodeHandoff writes a raw block volume on client node
// A and reads it on client node B.
func TestISCSIRawBlockCrossNodeHandoff(t *testing.T) {
	cfg := loadISCSIConfig(t)
	ns := createNamespace(t, "iscsi-block")
	defer deleteNamespace(t, ns)
	createISCSIPVC(t, ns, "raw", cfg.storageClass, "Block", iscsiVolumeSize)

	createRawBlockPod(t, ns, "raw-writer", "raw", cfg.clientNodeA)
	waitForPodReady(t, ns, "raw-writer")
	assertPodNode(t, ns, "raw-writer", cfg.clientNodeA)
	target := readPVISCSITarget(t, ns, "raw", cfg.targetAddress)
	t.Cleanup(func() { waitForLIOTargetRemoved(t, target) })
	kubectl(t, "-n", ns, "exec", "raw-writer", "--", "sh", "-c", "test -b /dev/pillar")
	requirePodUsesISCSIDevice(t, ns, "raw-writer", cfg.clientNodeA, target, readISCSIInitiatorIQN(t, cfg.clientNodeA), true)

	const marker = "pillar-csi-iscsi-raw-block"
	kubectl(t, "-n", ns, "exec", "raw-writer", "--", "sh", "-c",
		fmt.Sprintf("printf '%%s' %q | dd of=/dev/pillar bs=1 conv=fsync", marker))

	verifyISCSICrossNodeHandoff(t, ns, "raw-writer", "raw-reader", target, cfg.clientNodeA, cfg.clientNodeB, func() {
		createRawBlockPod(t, ns, "raw-reader", "raw", cfg.clientNodeB)
	})
	requirePodUsesISCSIDevice(t, ns, "raw-reader", cfg.clientNodeB, target, readISCSIInitiatorIQN(t, cfg.clientNodeB), true)
	got := kubectl(t, "-n", ns, "exec", "raw-reader", "--", "sh", "-c",
		fmt.Sprintf("dd if=/dev/pillar bs=1 count=%d", len(marker)))
	if got != marker {
		t.Fatalf("cross-node raw block read %q, want %q", got, marker)
	}
}

// TestISCSIOnlineFilesystemExpansion grows a mounted iSCSI filesystem
// volume: the LV grows on the agent, the initiator rescans the LUN and the
// node resizes the mounted filesystem.
func TestISCSIOnlineFilesystemExpansion(t *testing.T) {
	cfg := loadISCSIConfig(t)
	ns := createNamespace(t, "iscsi-expand")
	defer deleteNamespace(t, ns)
	createISCSIPVC(t, ns, "expandable", cfg.storageClass, "Filesystem", iscsiVolumeSize)

	createFilesystemPod(t, ns, "expander", "expandable", cfg.clientNodeA)
	waitForPodReady(t, ns, "expander")
	target := readPVISCSITarget(t, ns, "expandable", cfg.targetAddress)
	t.Cleanup(func() { waitForLIOTargetRemoved(t, target) })
	iqnA := readISCSIInitiatorIQN(t, cfg.clientNodeA)
	device := requirePodUsesISCSIDevice(t, ns, "expander", cfg.clientNodeA, target, iqnA, false)
	kubectl(t, "-n", ns, "exec", "expander", "--", "sh", "-c", "printf expansion-data > /data/payload && sync")

	beforeFS := filesystemBytes(t, ns, "expander")
	beforeDevice := blockDeviceBytes(t, device)
	kubectl(t, "-n", ns, "patch", "pvc", "expandable", "--type=merge", "-p",
		`{"spec":{"resources":{"requests":{"storage":"128Mi"}}}}`)

	waitFor(t, "PVC capacity to reach 128Mi", func() (bool, string) {
		quantity := kubectl(t, "-n", ns, "get", "pvc", "expandable", "-o", "jsonpath={.status.capacity.storage}")
		return quantityBytes(quantity) >= 128*1024*1024, quantity
	})
	waitFor(t, "the initiator's SCSI disk to grow", func() (bool, string) {
		after := blockDeviceBytes(t, device)
		return after >= 128*1024*1024 && after > beforeDevice,
			fmt.Sprintf("%s before=%d after=%d", device, beforeDevice, after)
	})
	waitFor(t, "mounted filesystem to grow", func() (bool, string) {
		after := filesystemBytes(t, ns, "expander")
		return after > beforeFS, fmt.Sprintf("before=%d after=%d", beforeFS, after)
	})
	if got := kubectl(t, "-n", ns, "exec", "expander", "--", "cat", "/data/payload"); got != "expansion-data" {
		t.Fatalf("payload after expansion = %q, want expansion-data", got)
	}
}

// TestISCSIFilesystemTrimReleasesSpace deletes a file on a mounted iSCSI
// filesystem, runs fstrim and checks the freed blocks reach the storage
// backend: the target advertises UNMAP (the client's SCSI disk has a
// non-zero discard limit) and the discards travel LIO iblock -> LV -> loop
// device -> backing file.
//
// The E2E LVM store is a linear LV in a VG whose only PV is a loop device
// over a sparse file.  A linear LV's size in lvs(8) never changes, but the
// loop driver punches a hole in its backing file for every discard, so the
// file's allocated blocks (st_blocks) are the space the volume really holds
// on the storage host.  Recycled extents of earlier volumes may already be
// allocated, so only the drop after fstrim is asserted, not the growth.
func TestISCSIFilesystemTrimReleasesSpace(t *testing.T) {
	cfg := loadISCSIConfig(t)
	backingContainer := requireEnv(t, "PILLAR_E2E_BACKING_CONTAINER")
	backingFile := requireEnv(t, "PILLAR_E2E_BACKING_FILE")
	ns := createNamespace(t, "iscsi-trim")
	defer deleteNamespace(t, ns)
	createISCSIPVC(t, ns, "data", cfg.storageClass, "Filesystem", iscsiTrimVolumeSize)

	createFilesystemPod(t, ns, "trimmer", "data", cfg.clientNodeA)
	waitForPodReady(t, ns, "trimmer")
	assertPodNode(t, ns, "trimmer", cfg.clientNodeA)
	target := readPVISCSITarget(t, ns, "data", cfg.targetAddress)
	t.Cleanup(func() { waitForLIOTargetRemoved(t, target) })
	requireLIOTarget(t, target)
	iqnA := readISCSIInitiatorIQN(t, cfg.clientNodeA)
	disk := requirePodUsesISCSIDevice(t, ns, "trimmer", cfg.clientNodeA, target, iqnA, false)

	if limit := blockDeviceDiscardMaxBytes(t, disk); limit == 0 {
		t.Fatalf(
			"iSCSI disk /dev/%s of target %s has queue/discard_max_bytes = 0: the target does not "+
				"advertise UNMAP (LIO backstore attrib/emulate_tpu = %s)",
			disk, target.iqn, lioBackstoreAttribute(target, "emulate_tpu"),
		)
	}

	const payloadBytes = iscsiTrimPayloadMiB * 1024 * 1024
	baseline := backingAllocatedBytes(t, backingContainer, backingFile)
	kubectl(t, "-n", ns, "exec", "trimmer", "--", "sh", "-c", fmt.Sprintf(
		"dd if=/dev/urandom of=/data/trim-payload bs=1M count=%d conv=fsync && sync", iscsiTrimPayloadMiB,
	))
	written := backingAllocatedBytes(t, backingContainer, backingFile)
	t.Logf("%s:%s allocated %d bytes before and %d bytes after writing %d bytes",
		backingContainer, backingFile, baseline, written, payloadBytes)

	// sync commits the journal transaction that frees the blocks; ext4 and
	// xfs trim only committed free space.
	kubectl(t, "-n", ns, "exec", "trimmer", "--", "sh", "-c", "rm /data/trim-payload && sync")
	mountPath := podVolumeMountPath(t, ns, "trimmer", "data")
	mount := dockerExec(t, cfg.clientNodeA, "findmnt", "-n", "-o", "SOURCE,FSTYPE", "--mountpoint", mountPath)
	t.Logf("%s mounts %s at %s", cfg.clientNodeA, mount, mountPath)
	t.Logf("fstrim on %s: %s", cfg.clientNodeA, dockerExec(t, cfg.clientNodeA, "fstrim", "-v", mountPath))

	trimmed := backingAllocatedBytes(t, backingContainer, backingFile)
	t.Logf("%s:%s allocated %d bytes after fstrim", backingContainer, backingFile, trimmed)
	if released := written - trimmed; released < payloadBytes/2 {
		t.Fatalf(
			"fstrim released %d bytes of %s:%s (allocated %d -> %d), want at least %d of the %d deleted bytes",
			released, backingContainer, backingFile, written, trimmed, payloadBytes/2, payloadBytes,
		)
	}
}

// TestISCSIUnauthorizedInitiatorRejected checks the acl: true contract while
// client node A holds the volume: only A's IQN has a node ACL, a login with
// client node B's IQN is refused by the target, and A keeps working.
func TestISCSIUnauthorizedInitiatorRejected(t *testing.T) {
	cfg := loadISCSIConfig(t)
	ns := createNamespace(t, "iscsi-acl")
	defer deleteNamespace(t, ns)
	createISCSIPVC(t, ns, "data", cfg.storageClass, "Filesystem", iscsiVolumeSize)

	createFilesystemPod(t, ns, "owner", "data", cfg.clientNodeA)
	waitForPodReady(t, ns, "owner")
	target := readPVISCSITarget(t, ns, "data", cfg.targetAddress)
	t.Cleanup(func() { waitForLIOTargetRemoved(t, target) })
	iqnA := readISCSIInitiatorIQN(t, cfg.clientNodeA)
	iqnB := readISCSIInitiatorIQN(t, cfg.clientNodeB)
	if iqnA == iqnB {
		t.Fatalf("Kind clients %q and %q share initiator IQN %q", cfg.clientNodeA, cfg.clientNodeB, iqnA)
	}
	requireISCSIConnected(t, cfg.clientNodeA, target, iqnA)
	waitForLIOACLs(t, target, []string{iqnA})

	requireTCPReachable(t, cfg.clientNodeB, target.address, target.port)
	assertUnauthorizedISCSILoginRejected(t, cfg.clientNodeB, target, iqnB)
	requireISCSIDisconnected(t, cfg.clientNodeB, target, iqnB)
	waitForLIOACLs(t, target, []string{iqnA})

	kubectl(t, "-n", ns, "exec", "owner", "--", "sh", "-c", "printf still-served > /data/payload && sync")
	if got := kubectl(t, "-n", ns, "exec", "owner", "--", "cat", "/data/payload"); got != "still-served" {
		t.Fatalf("authorized node read %q after the rejected login, want still-served", got)
	}
	requireISCSIConnected(t, cfg.clientNodeA, target, iqnA)
}

// TestISCSIProtocolAdmission checks the API server rejects a PillarProtocol
// with two protocol members and an iscsi override on a class whose protocol
// is nvmeofTcp, and admits an iscsi override on the iscsi protocol.
func TestISCSIProtocolAdmission(t *testing.T) {
	cfg := loadISCSIConfig(t)

	err := dryRunApply(t, `apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: pillar-e2e-two-members
spec:
  protocol:
    nvmeofTcp:
      port: 4420
    iscsi:
      port: 3260
`)
	if err == nil || !strings.Contains(err.Error(), "exactly one protocol member must be set") {
		t.Fatalf("PillarProtocol with nvmeofTcp and iscsi: err = %v, want the exactly-one-member rejection", err)
	}

	const classTemplate = `apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: %s
spec:
  storeRef: pillar-e2e-store
  protocolRef: %s
  overrides:
    protocol:
      iscsi:
        loginTimeout: 20
`
	err = dryRunApply(t, fmt.Sprintf(classTemplate, "pillar-e2e-iscsi-on-nvme", "pillar-e2e-nvme"))
	if err == nil || !strings.Contains(err.Error(), `protocol override member "iscsi" does not match`) {
		t.Fatalf("iscsi override on the nvmeofTcp protocol: err = %v, want the member mismatch rejection", err)
	}
	if err := dryRunApply(t, fmt.Sprintf(classTemplate, "pillar-e2e-iscsi-on-iscsi", cfg.protocol)); err != nil {
		t.Fatalf("iscsi override on the iscsi protocol rejected: %v", err)
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────

// iscsiVolumeSize is the requested size of the iSCSI test volumes.
const iscsiVolumeSize = "64Mi"

// iscsiTrimVolumeSize and iscsiTrimPayloadMiB size the trim test: the
// payload must fit the ext4 volume and dwarf filesystem metadata churn.
const (
	iscsiTrimVolumeSize = "256Mi"
	iscsiTrimPayloadMiB = 128
)

func createISCSIPVC(t *testing.T, namespace, name, storageClass, volumeMode, size string) {
	t.Helper()
	apply(t, fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: %s
  storageClassName: %s
  resources:
    requests:
      storage: %s
`, name, namespace, volumeMode, storageClass, size))
}

// dryRunApply submits manifest to the API server (admission included)
// without persisting it and returns the server's error.
func dryRunApply(t *testing.T, manifest string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest %s: %v", path, err)
	}
	_, stderr, err := runKubectl(ctx, "apply", "--dry-run=server", "-f", path)
	if err != nil {
		return fmt.Errorf("%w: %s", err, stderr)
	}
	return nil
}

// readPVISCSITarget waits for claim to bind and returns the iSCSI target of
// its PV, checking the VolumeContext the controller produced.
func readPVISCSITarget(t *testing.T, namespace, claim, wantAddress string) iscsiTarget {
	t.Helper()
	waitFor(t, "PVC to bind", func() (bool, string) {
		phase := kubectl(t, "-n", namespace, "get", "pvc", claim, "-o", "jsonpath={.status.phase}")
		return phase == "Bound", phase
	})
	pv := kubectl(t, "-n", namespace, "get", "pvc", claim, "-o", "jsonpath={.spec.volumeName}")
	attr := func(key string) string {
		return kubectl(t, "get", "pv", pv, "-o",
			"jsonpath={.spec.csi.volumeAttributes."+strings.ReplaceAll(key, ".", `\.`)+"}")
	}
	if got := attr("pillar-csi.bhyoo.com/protocol-type"); got != "iscsi" {
		t.Fatalf("PV %s protocol-type = %q, want iscsi", pv, got)
	}
	iqn := attr("target_id")
	if !strings.HasPrefix(iqn, iscsiOwnedIQNPrefix) {
		t.Fatalf("PV %s target_id = %q, want an IQN starting with %s", pv, iqn, iscsiOwnedIQNPrefix)
	}
	address := attr("address")
	if address != wantAddress {
		t.Fatalf("PV %s target address = %q, want the storage node %q", pv, address, wantAddress)
	}
	port := attr("port")
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		t.Fatalf("PV %s target port = %q, want a TCP port: %v", pv, port, err)
	}
	targetAddress, err := netip.ParseAddr(address)
	if err != nil {
		t.Fatalf("PV %s target address = %q, want an IP address: %v", pv, address, err)
	}
	target := iscsiTarget{
		iqn:      iqn,
		address:  address,
		port:     port,
		endpoint: netip.AddrPortFrom(targetAddress.Unmap(), uint16(portNumber)),
	}
	requireLIOTarget(t, target)
	return target
}

// requireLIOTarget checks the agent built the target in the (host-global)
// LIO configfs: an enabled TPG with LUN 0 and a network portal on the port.
func requireLIOTarget(t *testing.T, target iscsiTarget) {
	t.Helper()
	tpg := filepath.Join(lioISCSIRoot, target.iqn, lioTPG)
	enable, err := os.ReadFile(filepath.Join(tpg, "enable"))
	if err != nil {
		t.Fatalf("read LIO TPG enable of %s: %v", target.iqn, err)
	}
	if strings.TrimSpace(string(enable)) != "1" {
		t.Fatalf("LIO TPG of %s enable = %q, want 1", target.iqn, strings.TrimSpace(string(enable)))
	}
	if _, err := os.Stat(filepath.Join(tpg, "lun", "lun_0")); err != nil {
		t.Fatalf("LIO target %s has no LUN 0: %v", target.iqn, err)
	}
	portals, err := os.ReadDir(filepath.Join(tpg, "np"))
	if err != nil {
		t.Fatalf("list LIO network portals of %s: %v", target.iqn, err)
	}
	for _, portal := range portals {
		if strings.HasSuffix(portal.Name(), ":"+target.port) {
			return
		}
	}
	t.Fatalf("LIO target %s has no network portal on port %s: %v", target.iqn, target.port, dirNames(portals))
}

// waitForLIOACLs waits until the node ACLs of target are exactly want.
func waitForLIOACLs(t *testing.T, target iscsiTarget, want []string) {
	t.Helper()
	want = slices.Sorted(slices.Values(want))
	waitFor(t, fmt.Sprintf("LIO node ACLs of %s to be %v", target.iqn, want), func() (bool, string) {
		entries, err := os.ReadDir(filepath.Join(lioISCSIRoot, target.iqn, lioTPG, "acls"))
		if err != nil {
			return false, err.Error()
		}
		got := dirNames(entries)
		slices.Sort(got)
		return slices.Equal(got, want), fmt.Sprintf("acls=%v", got)
	})
}

// waitForLIOTargetRemoved waits until the deleted volume left no LIO target
// and no iblock backstore behind.
func waitForLIOTargetRemoved(t *testing.T, target iscsiTarget) {
	t.Helper()
	targetDir := filepath.Join(lioISCSIRoot, target.iqn)
	backstore := filepath.Join(lioIBlockHBA, strings.TrimPrefix(target.iqn, iscsiOwnedIQNPrefix))
	waitFor(t, "LIO target "+target.iqn+" and its backstore to be removed", func() (bool, string) {
		var remaining []string
		for _, path := range []string{targetDir, backstore} {
			if _, err := os.Lstat(path); err == nil {
				remaining = append(remaining, path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return false, err.Error()
			}
		}
		return len(remaining) == 0, fmt.Sprintf("remaining=%v", remaining)
	})
}

func dirNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// readISCSIInitiatorIQN returns the initiator IQN pillar-node persisted on
// node and checks it matches the node's CSINode annotation.
func readISCSIInitiatorIQN(t *testing.T, node string) string {
	t.Helper()
	iqn := dockerExec(t, node, "sed", "-n", "s/^InitiatorName=//p", "/etc/iscsi/initiatorname.iscsi")
	if !strings.HasPrefix(iqn, "iqn.") || strings.ContainsAny(iqn, " \n") {
		t.Fatalf("Kind node %q initiator IQN = %q, want one iqn. name", node, iqn)
	}
	annotation := kubectl(t, "get", "csinode", node, "-o",
		`jsonpath={.metadata.annotations.pillar-csi\.bhyoo\.com/iscsi-initiator-iqn}`)
	if annotation != iqn {
		t.Fatalf("Kind node %q CSINode iscsi-initiator-iqn = %q, want %q from /etc/iscsi", node, annotation, iqn)
	}
	return iqn
}

type iscsiSessionState struct {
	sid         string
	targetName  string
	initiator   string
	state       string
	connAddress string
	connPort    string
	devices     []iscsiDevice
}

type iscsiDevice struct {
	name   string // sdX
	majMin string // decimal major:minor
}

type iscsiNodeState struct {
	networkNamespace string
	sessions         []iscsiSessionState // sessions of the node's initiator to the target
	targetSockets    []netip.AddrPort
	inspectionErrors []string
}

// readISCSINodeState inspects the sessions of initiator iqn to target and
// the node network namespace's TCP connections to the target portal.
func readISCSINodeState(t *testing.T, node string, target iscsiTarget, iqn string) iscsiNodeState {
	t.Helper()
	const inspectScript = `set -u
printf 'netns\t%s\n' "$(readlink /proc/self/ns/net)"
for s in /sys/class/iscsi_session/session*; do
  [ -d "$s" ] || continue
  sid=${s##*/session}
  tn=$(cat "$s/targetname" 2>/dev/null) || { printf 'error\tread %s/targetname\n' "$s"; continue; }
  [ "$tn" = "$1" ] || continue
  in=$(cat "$s/initiatorname" 2>/dev/null) || { printf 'error\tread %s/initiatorname\n' "$s"; continue; }
  [ "$in" = "$2" ] || continue
  st=$(cat "$s/state" 2>/dev/null) || st=unknown
  addr=""; port=""
  for c in /sys/class/iscsi_connection/connection"$sid":*; do
    [ -d "$c" ] || continue
    addr=$(cat "$c/address" 2>/dev/null) || addr=""
    port=$(cat "$c/port" 2>/dev/null) || port=""
  done
  printf 'session\t%s\t%s\t%s\t%s\t%s\t%s\n' "$sid" "$tn" "$in" "$st" "$addr" "$port"
  for b in "$s"/device/target*/*/block/*; do
    [ -e "$b" ] || continue
    name=${b##*/}
    dev=$(cat "/sys/class/block/$name/dev" 2>/dev/null) || { printf 'error\tread /sys/class/block/%s/dev\n' "$name"; continue; }
    printf 'device\t%s\t%s\t%s\n' "$sid" "$name" "$dev"
  done
done`
	output := dockerExec(t, node, "sh", "-c", inspectScript, "inspect-iscsi", target.iqn, iqn)
	state := iscsiNodeState{}
	bySID := map[string]int{}
	for line := range strings.SplitSeq(output, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		switch fields[0] {
		case "netns":
			if len(fields) != 2 || fields[1] == "" {
				t.Fatalf("parse iSCSI state from Kind node %q: invalid netns record %q", node, line)
			}
			state.networkNamespace = fields[1]
		case "session":
			if len(fields) != 7 {
				t.Fatalf("parse iSCSI state from Kind node %q: invalid session record %q", node, line)
			}
			state.sessions = append(state.sessions, iscsiSessionState{
				sid: fields[1], targetName: fields[2], initiator: fields[3], state: fields[4],
				connAddress: fields[5], connPort: fields[6],
			})
			bySID[fields[1]] = len(state.sessions) - 1
		case "device":
			if len(fields) != 4 {
				t.Fatalf("parse iSCSI state from Kind node %q: invalid device record %q", node, line)
			}
			index, ok := bySID[fields[1]]
			if !ok {
				t.Fatalf("parse iSCSI state from Kind node %q: device of unknown session %q", node, line)
			}
			state.sessions[index].devices = append(state.sessions[index].devices,
				iscsiDevice{name: fields[2], majMin: fields[3]})
		case "error":
			if len(fields) != 2 {
				t.Fatalf("parse iSCSI state from Kind node %q: invalid error record %q", node, line)
			}
			state.inspectionErrors = append(state.inspectionErrors, fields[1])
		default:
			t.Fatalf("parse iSCSI state from Kind node %q: unknown record %q", node, line)
		}
	}
	if state.networkNamespace == "" {
		t.Fatalf("iSCSI state from Kind node %q did not report its network namespace", node)
	}
	const socketsScript = `set -eu
cat /proc/net/tcp
if [ -r /proc/net/tcp6 ]; then
  cat /proc/net/tcp6
fi`
	endpoints, err := parseEstablishedTCPEndpoints(dockerExec(t, node, "sh", "-ceu", socketsScript))
	if err != nil {
		t.Fatalf("parse established TCP sockets in Kind node %q: %v", node, err)
	}
	for _, endpoint := range endpoints {
		if endpoint == target.endpoint {
			state.targetSockets = append(state.targetSockets, endpoint)
		}
	}
	return state
}

func (s iscsiNodeState) String() string {
	return fmt.Sprintf("netns=%q sessions=%+v target-sockets=%v inspection-errors=%v",
		s.networkNamespace, s.sessions, s.targetSockets, s.inspectionErrors)
}

// iscsiConnectedState reports whether the node's initiator has exactly one
// logged-in session to target at the target portal, backed by an sd disk and
// a TCP connection from the node's own network namespace.
func iscsiConnectedState(target iscsiTarget, state iscsiNodeState) (bool, string) {
	if len(state.inspectionErrors) != 0 {
		return false, fmt.Sprintf("iSCSI state inspection errors: %v", state.inspectionErrors)
	}
	if len(state.sessions) != 1 {
		return false, fmt.Sprintf("want one session to %s, found %d", target.iqn, len(state.sessions))
	}
	s := state.sessions[0]
	if s.state != "LOGGED_IN" {
		return false, fmt.Sprintf("session %s state %q, want LOGGED_IN", s.sid, s.state)
	}
	if s.connAddress != target.address || s.connPort != target.port {
		return false, fmt.Sprintf("session %s connected to %s:%s, want the storage node %s:%s",
			s.sid, s.connAddress, s.connPort, target.address, target.port)
	}
	if len(s.devices) != 1 || !strings.HasPrefix(s.devices[0].name, "sd") {
		return false, fmt.Sprintf("session %s devices %+v, want one sd disk (LUN 0)", s.sid, s.devices)
	}
	if len(state.targetSockets) == 0 {
		return false, fmt.Sprintf("network namespace has no established TCP connection to %s", target.endpoint)
	}
	return true, ""
}

func iscsiDisconnectedState(target iscsiTarget, state iscsiNodeState) (bool, string) {
	if len(state.inspectionErrors) != 0 {
		return false, fmt.Sprintf("iSCSI state inspection errors: %v", state.inspectionErrors)
	}
	if len(state.sessions) != 0 {
		return false, fmt.Sprintf("sessions to %s remain: %+v", target.iqn, state.sessions)
	}
	if len(state.targetSockets) != 0 {
		return false, fmt.Sprintf("established TCP connection to %s remains", target.endpoint)
	}
	return true, ""
}

func requireISCSIConnected(t *testing.T, node string, target iscsiTarget, iqn string) {
	t.Helper()
	state := readISCSINodeState(t, node, target, iqn)
	if ok, reason := iscsiConnectedState(target, state); !ok {
		t.Fatalf("Kind node %q is not logged in to iSCSI target %s: %s; %s", node, target.iqn, reason, state)
	}
}

func requireISCSIDisconnected(t *testing.T, node string, target iscsiTarget, iqn string) {
	t.Helper()
	state := readISCSINodeState(t, node, target, iqn)
	if ok, reason := iscsiDisconnectedState(target, state); !ok {
		t.Fatalf("Kind node %q is still logged in to iSCSI target %s: %s; %s", node, target.iqn, reason, state)
	}
}

func waitForISCSIDetached(t *testing.T, node string, target iscsiTarget, iqn string) {
	t.Helper()
	waitFor(t, fmt.Sprintf("iSCSI target %q to log out from Kind node %q", target.iqn, node), func() (bool, string) {
		state := readISCSINodeState(t, node, target, iqn)
		ok, reason := iscsiDisconnectedState(target, state)
		return ok, reason + "; " + state.String()
	})
}

// requirePodUsesISCSIDevice checks that pod on node uses the SCSI disk of
// the node's session to target (mounted at /data, or /dev/pillar for raw
// block) and returns the disk name.
func requirePodUsesISCSIDevice(
	t *testing.T, namespace, pod, node string, target iscsiTarget, iqn string, rawBlock bool,
) string {
	t.Helper()
	state := readISCSINodeState(t, node, target, iqn)
	if ok, reason := iscsiConnectedState(target, state); !ok {
		t.Fatalf("Kind node %q is not logged in to iSCSI target %s: %s; %s", node, target.iqn, reason, state)
	}
	disk := state.sessions[0].devices[0]
	var got string
	if rawBlock {
		got = podBlockDevice(t, namespace, pod)
	} else {
		got = podMountDevice(t, namespace, pod)
	}
	if got != disk.majMin {
		t.Fatalf("pod %s/%s uses device %s, want iSCSI disk /dev/%s (%s)", namespace, pod, got, disk.name, disk.majMin)
	}
	return disk.name
}

// podMountFSType returns the filesystem type mounted at /data in pod.
func podMountFSType(t *testing.T, namespace, pod string) string {
	t.Helper()
	return kubectl(t, "-n", namespace, "exec", pod, "--", "awk", `$2 == "/data" { print $3 }`, "/proc/mounts")
}

// blockDeviceBytes returns the size of the host block device name.
func blockDeviceBytes(t *testing.T, name string) int64 {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("/sys/class/block", name, "size"))
	if err != nil {
		t.Fatalf("read size of block device %s: %v", name, err)
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		t.Fatalf("parse size of block device %s: %v", name, err)
	}
	return sectors * 512
}

// blockDeviceDiscardMaxBytes returns the discard limit of the host block
// device name; 0 means the device does not support discard.
func blockDeviceDiscardMaxBytes(t *testing.T, name string) int64 {
	t.Helper()
	path := filepath.Join("/sys/class/block", name, "queue", "discard_max_bytes")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read discard limit of block device %s: %v", name, err)
	}
	limit, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return limit
}

// lioBackstoreAttribute returns the value of attribute name of target's
// iblock backstore in the (host-global) LIO configfs, for diagnostics.
func lioBackstoreAttribute(target iscsiTarget, name string) string {
	backstore := filepath.Join(lioIBlockHBA, strings.TrimPrefix(target.iqn, iscsiOwnedIQNPrefix))
	raw, err := os.ReadFile(filepath.Join(backstore, "attrib", name))
	if err != nil {
		return fmt.Sprintf("unreadable: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

// podVolumeMountPath returns the kubelet publish path of volume in pod on
// the pod's node.
func podVolumeMountPath(t *testing.T, namespace, pod, claim string) string {
	t.Helper()
	uid := kubectl(t, "-n", namespace, "get", "pod", pod, "-o", "jsonpath={.metadata.uid}")
	pv := kubectl(t, "-n", namespace, "get", "pvc", claim, "-o", "jsonpath={.spec.volumeName}")
	return "/var/lib/kubelet/pods/" + uid + "/volumes/kubernetes.io~csi/" + pv + "/mount"
}

// backingAllocatedBytes flushes container's page cache and returns the
// bytes allocated to file there (st_blocks, not the apparent size).
func backingAllocatedBytes(t *testing.T, container, file string) int64 {
	t.Helper()
	output := dockerExec(t, container, "sh", "-c", `sync && stat -c '%b %B' "$1"`, "backing-usage", file)
	fields := strings.Fields(output)
	if len(fields) != 2 {
		t.Fatalf("stat of %s:%s = %q, want \"<blocks> <block size>\"", container, file, output)
	}
	blocks, blocksErr := strconv.ParseInt(fields[0], 10, 64)
	blockSize, sizeErr := strconv.ParseInt(fields[1], 10, 64)
	if blocksErr != nil || sizeErr != nil {
		t.Fatalf("parse stat of %s:%s %q: %v", container, file, output, errors.Join(blocksErr, sizeErr))
	}
	return blocks * blockSize
}

// verifyISCSICrossNodeHandoff checks the handoff of a volume from client A
// to client B: B has no session and is refused by the target's ACL while A
// holds the volume; after the writer pod is gone A logs out, B logs in, and
// the node ACL follows the volume from A to B.
func verifyISCSICrossNodeHandoff(
	t *testing.T,
	namespace, writerPod, readerPod string,
	target iscsiTarget,
	clientA, clientB string,
	createReader func(),
) {
	t.Helper()
	iqnA := readISCSIInitiatorIQN(t, clientA)
	iqnB := readISCSIInitiatorIQN(t, clientB)
	if iqnA == iqnB {
		t.Fatalf("Kind clients %q and %q share initiator IQN %q", clientA, clientB, iqnA)
	}
	writerState := readISCSINodeState(t, clientA, target, iqnA)
	readerState := readISCSINodeState(t, clientB, target, iqnB)
	if writerState.networkNamespace == readerState.networkNamespace {
		t.Fatalf("Kind clients %q and %q share network namespace %q; cannot verify initiator isolation",
			clientA, clientB, writerState.networkNamespace)
	}
	requireISCSIConnected(t, clientA, target, iqnA)
	requireISCSIDisconnected(t, clientB, target, iqnB)
	waitForLIOACLs(t, target, []string{iqnA})

	requireTCPReachable(t, clientB, target.address, target.port)
	assertUnauthorizedISCSILoginRejected(t, clientB, target, iqnB)
	requireISCSIDisconnected(t, clientB, target, iqnB)

	kubectl(t, "-n", namespace, "delete", "pod", writerPod, "--wait=true", "--timeout=3m")
	waitForISCSIDetached(t, clientA, target, iqnA)

	createReader()
	waitForPodReady(t, namespace, readerPod)
	assertPodNode(t, namespace, readerPod, clientB)
	requireISCSIConnected(t, clientB, target, iqnB)
	requireISCSIDisconnected(t, clientA, target, iqnA)
	waitForLIOACLs(t, target, []string{iqnB})
}

// requireTCPReachable checks node can open a TCP connection to address:port,
// so a later login rejection is the target's answer and not a network fault.
func requireTCPReachable(t *testing.T, node, address, port string) {
	t.Helper()
	const reachedMarker = "target-tcp-reachable"
	const script = `set -eu
timeout 5 bash -ceu ': >"/dev/tcp/$1/$2"' "tcp-reachability" "$1" "$2"
printf '%s\n' '` + reachedMarker + `'`
	ctx, cancel := context.WithTimeout(context.Background(), iscsiLoginTimeout)
	defer cancel()
	stdout, stderr, err := runDockerExec(ctx, node, "bash", "-ceu", script, "probe-tcp", address, port)
	if err != nil || stdout != reachedMarker {
		t.Fatalf("target TCP endpoint %s:%s is not reachable from Kind node %q before the ACL probe: %v\nstdout:\n%s\nstderr:\n%s",
			address, port, node, err, stdout, stderr)
	}
}

// iscsiLoginRequest returns the first Login Request PDU (RFC 7143 §11.12)
// of a normal session from initiator to target: security negotiation stage
// with a transit to operational negotiation and AuthMethod=None.
func iscsiLoginRequest(initiator, target string) []byte {
	data := []byte("InitiatorName=" + initiator + "\x00SessionType=Normal\x00TargetName=" + target +
		"\x00AuthMethod=None\x00")
	dataLen := len(data)
	for len(data)%4 != 0 {
		data = append(data, 0)
	}
	pdu := make([]byte, 48, 48+len(data))
	pdu[0] = 0x40 | 0x03 // immediate, Login Request
	pdu[1] = 0x80 | 0x01 // Transit, CSG=security(0), NSG=operational(1)
	pdu[5] = byte(dataLen >> 16)
	pdu[6] = byte(dataLen >> 8)
	pdu[7] = byte(dataLen)
	copy(pdu[8:14], []byte{0x80, 0x00, 0x70, 0x11, 0xe2, 0xe0}) // ISID: random format
	binary.BigEndian.PutUint32(pdu[16:], 0x0e2e)                // Initiator Task Tag
	return append(pdu, data...)
}

// assertUnauthorizedISCSILoginRejected sends a login for target with
// initiator iqn from node's network namespace and requires the target to
// refuse it as forbidden (no node ACL).
func assertUnauthorizedISCSILoginRejected(t *testing.T, node string, target iscsiTarget, iqn string) {
	t.Helper()
	var escaped strings.Builder
	for _, b := range iscsiLoginRequest(iqn, target.iqn) {
		fmt.Fprintf(&escaped, `\x%02x`, b)
	}
	const loginScript = `set -eu
exec 3<>"/dev/tcp/$1/$2"
printf '%b' "$3" >&3
timeout 10 head -c 48 <&3 | od -An -v -tx1`
	ctx, cancel := context.WithTimeout(context.Background(), iscsiLoginTimeout)
	defer cancel()
	stdout, stderr, err := runDockerExec(ctx, node, "bash", "-ceu", loginScript, "unauthorized-iscsi-login",
		target.address, target.port, escaped.String())
	if err != nil {
		t.Fatalf("unauthorized iSCSI login from Kind node %q to %s failed before a response: %v\nstdout:\n%s\nstderr:\n%s",
			node, target.endpoint, err, stdout, stderr)
	}
	response, err := hex.DecodeString(strings.Join(strings.Fields(stdout), ""))
	if err != nil || len(response) != 48 {
		t.Fatalf("unauthorized iSCSI login from Kind node %q: want a 48-byte login response header, got %q (%v)",
			node, stdout, err)
	}
	if opcode := response[0] & 0x3f; opcode != 0x23 {
		t.Fatalf("unauthorized iSCSI login from Kind node %q: response opcode %#x, want Login Response 0x23", node, opcode)
	}
	class, detail := response[36], response[37]
	if class == 0 {
		t.Fatalf("iSCSI target %s accepted a login from unauthorized initiator %q on Kind node %q", target.iqn, iqn, node)
	}
	if class != iscsiStatusClassInitiatorError || detail != iscsiStatusDetailForbidden {
		t.Fatalf("unauthorized iSCSI login from Kind node %q: status class %#x detail %#x, want %#x/%#x (target forbidden)",
			node, class, detail, iscsiStatusClassInitiatorError, iscsiStatusDetailForbidden)
	}
}
