//go:build docker_e2e

package dockere2e

// trim_e2e_test.go — pillar-node's periodic filesystem trim on real kernels.
//
// run.sh installs the chart with node.trim.interval=PILLAR_E2E_TRIM_INTERVAL
// (seconds, not the weekly default), so a deleted file's blocks must reach
// the storage backend without anyone running fstrim.  The E2E LVM store is a
// linear LV on a loop device over a sparse file: a discard that reaches the
// LV punches a hole in the backing file, so the file's allocated blocks
// (st_blocks) are the space the volumes really hold.  The backing file is
// shared by every LV, so the per-volume signal is the LV's own dm device
// discard counter (/sys/dev/block/<maj:min>/stat); Kind nodes and the external
// agent share the host kernel, so the test reads it directly.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// periodicTrimPayloadMiB is written incompressibly into each volume and
	// deleted again; it must dwarf the filesystem metadata churn.
	periodicTrimPayloadMiB = 128
	periodicTrimVolumeSize = "256Mi"
	// periodicTrimShrinkTimeout bounds the wait for the trimmed volume's
	// space to return after the payload is deleted.
	periodicTrimShrinkTimeout = 3 * time.Minute
	periodicTrimPollInterval  = 5 * time.Second
	// periodicTrimAttribute is the VolumeContext key carrying a resolved
	// periodicTrim setting; it is emitted only when the setting is present.
	periodicTrimAttribute = "pillar-csi.bhyoo.com/periodic-trim"
	// blockStatDiscardSectorsField is the 0-based index of "sectors
	// discarded" in a block device's stat file (Documentation/block/stat.rst).
	blockStatDiscardSectorsField = 13
	blockStatSectorBytes         = 512
)

// TestPeriodicTrimReleasesSpace deletes a file on NVMe/TCP and iSCSI
// filesystem volumes and waits, without running fstrim, until pillar-node's
// periodic trim returns the space to the storage node.  A second volume in
// the same window sets periodicTrim: false in its PVC filesystem document and
// must receive no discard at all.
func TestPeriodicTrimReleasesSpace(t *testing.T) {
	cfg := loadConfig(t)
	iscsiStorageClass := requireEnv(t, "PILLAR_E2E_ISCSI_STORAGE_CLASS")
	backingContainer := requireEnv(t, "PILLAR_E2E_BACKING_CONTAINER")
	backingFile := requireEnv(t, "PILLAR_E2E_BACKING_FILE")
	interval, err := time.ParseDuration(requireEnv(t, "PILLAR_E2E_TRIM_INTERVAL"))
	if err != nil || interval <= 0 {
		t.Fatalf("PILLAR_E2E_TRIM_INTERVAL = %q, want a positive duration: %v",
			os.Getenv("PILLAR_E2E_TRIM_INTERVAL"), err)
	}
	// A volume enabled for trim whose last trim ran just before its payload
	// was deleted is due one interval later, and the node loop wakes at most
	// every min(interval, 1m).  Watching the opted-out volume for two
	// intervals after the delete therefore covers at least one trim it would
	// have received; the margin absorbs the trim itself.
	optOutWindow := 2*interval + 15*time.Second
	if optOutWindow > periodicTrimShrinkTimeout {
		t.Fatalf("trim interval %s is too long for the %s window; install the node with a shorter node.trim.interval",
			interval, periodicTrimShrinkTimeout)
	}

	for _, tc := range []struct {
		name         string
		storageClass string
		// requireTarget checks the claim went over the protocol under test.
		requireTarget func(t *testing.T, namespace, claim string)
	}{
		{
			name:         "nvmeof-tcp",
			storageClass: cfg.storageClass,
			requireTarget: func(t *testing.T, namespace, claim string) {
				t.Helper()
				readPVNVMeTarget(t, namespace, claim, cfg.targetAddress)
			},
		},
		{
			name:         "iscsi",
			storageClass: iscsiStorageClass,
			requireTarget: func(t *testing.T, namespace, claim string) {
				t.Helper()
				target := readPVISCSITarget(t, namespace, claim, cfg.targetAddress)
				t.Cleanup(func() { waitForLIOTargetRemoved(t, target) })
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := createNamespace(t, "trim-"+tc.name)
			defer deleteNamespace(t, ns)

			createAnnotatedFilesystemPVC(t, ns, "trimmed", tc.storageClass, periodicTrimVolumeSize, "")
			createAnnotatedFilesystemPVC(t, ns, "kept", tc.storageClass, periodicTrimVolumeSize, "periodicTrim: false")
			for _, name := range []string{"trimmed", "kept"} {
				createFilesystemPod(t, ns, name, name, cfg.clientNodeA)
			}
			for _, name := range []string{"trimmed", "kept"} {
				waitForPodReady(t, ns, name)
				assertPodNode(t, ns, name, cfg.clientNodeA)
				tc.requireTarget(t, ns, name)
			}
			if got := pvVolumeAttribute(t, ns, "trimmed", periodicTrimAttribute); got != "" {
				t.Fatalf("PV of claim trimmed has %s = %q, want it unset (node default)", periodicTrimAttribute, got)
			}
			if got := pvVolumeAttribute(t, ns, "kept", periodicTrimAttribute); got != "false" {
				t.Fatalf("PV of claim kept has %s = %q, want \"false\" from its PVC filesystem document",
					periodicTrimAttribute, got)
			}
			trimmedLV := lvKernelDevice(t, backingContainer, ns, "trimmed")
			keptLV := lvKernelDevice(t, backingContainer, ns, "kept")

			const payloadBytes = periodicTrimPayloadMiB * 1024 * 1024
			for _, pod := range []string{"trimmed", "kept"} {
				kubectl(t, "-n", ns, "exec", pod, "--", "sh", "-c", fmt.Sprintf(
					"dd if=/dev/urandom of=/data/trim-payload bs=1M count=%d conv=fsync && sync",
					periodicTrimPayloadMiB,
				))
			}
			written := backingAllocatedBytes(t, backingContainer, backingFile)
			trimmedBaseline := blockDeviceDiscardedBytes(t, trimmedLV)
			keptBaseline := blockDeviceDiscardedBytes(t, keptLV)

			// sync commits the journal transaction that frees the blocks:
			// ext4 trims only committed free space.
			for _, pod := range []string{"trimmed", "kept"} {
				kubectl(t, "-n", ns, "exec", pod, "--", "sh", "-c", "rm /data/trim-payload && sync")
			}
			deletedAt := time.Now()
			t.Logf("%s:%s allocated %d bytes with both %d-byte payloads written; deleted them at %s",
				backingContainer, backingFile, written, payloadBytes, deletedAt.Format(time.RFC3339))

			var shrunkAfter time.Duration
			for {
				elapsed := time.Since(deletedAt)
				if discarded := blockDeviceDiscardedBytes(t, keptLV) - keptBaseline; discarded != 0 {
					t.Fatalf("LV %s of claim kept (periodicTrim: false) received %d discarded bytes %s after the delete, want none",
						keptLV, discarded, elapsed.Round(time.Second))
				}
				allocated := backingAllocatedBytes(t, backingContainer, backingFile)
				if shrunkAfter == 0 && written-allocated >= payloadBytes/2 {
					shrunkAfter = elapsed
					t.Logf("%s:%s allocated %d bytes %s after the delete (released %d)",
						backingContainer, backingFile, allocated, elapsed.Round(time.Second), written-allocated)
				}
				if shrunkAfter != 0 && elapsed >= optOutWindow {
					break
				}
				if shrunkAfter == 0 && elapsed >= periodicTrimShrinkTimeout {
					t.Fatalf(
						"periodic trim released %d bytes of %s:%s within %s (allocated %d -> %d), want at least %d of the %d deleted bytes; "+
							"LV %s of claim trimmed received %d discarded bytes",
						written-allocated, backingContainer, backingFile, periodicTrimShrinkTimeout, written, allocated,
						payloadBytes/2, payloadBytes, trimmedLV, blockDeviceDiscardedBytes(t, trimmedLV)-trimmedBaseline,
					)
				}
				time.Sleep(periodicTrimPollInterval)
			}

			// The shrink must come from the trimmed volume: its LV saw the
			// discards, which also proves the counter the opted-out volume
			// is judged by does count discards.
			if discarded := blockDeviceDiscardedBytes(t, trimmedLV) - trimmedBaseline; discarded < payloadBytes/2 {
				t.Fatalf("LV %s of claim trimmed received %d discarded bytes, want at least %d",
					trimmedLV, discarded, payloadBytes/2)
			}
		})
	}
}

// createAnnotatedFilesystemPVC creates a filesystem PVC; a non-empty
// filesystemDoc (one YAML line) becomes its pillar-csi.bhyoo.com/filesystem
// annotation.
func createAnnotatedFilesystemPVC(t *testing.T, namespace, name, storageClass, size, filesystemDoc string) {
	t.Helper()
	annotations := ""
	if filesystemDoc != "" {
		annotations = "\n  annotations:\n    pillar-csi.bhyoo.com/filesystem: |\n      " + filesystemDoc
	}
	apply(t, fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s%s
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: %s
  resources:
    requests:
      storage: %s
`, name, namespace, annotations, storageClass, size))
}

// pvVolumeAttribute returns the VolumeContext value key of claim's PV, or ""
// when the PV does not carry it.
func pvVolumeAttribute(t *testing.T, namespace, claim, key string) string {
	t.Helper()
	pv := kubectl(t, "-n", namespace, "get", "pvc", claim, "-o", "jsonpath={.spec.volumeName}")
	return kubectl(t, "get", "pv", pv, "-o",
		"jsonpath={.spec.csi.volumeAttributes."+strings.ReplaceAll(key, ".", `\.`)+"}")
}

// lvKernelDevice returns the decimal major:minor of the LV backing claim, as
// seen by LVM in container.  The CSI volume handle is
// <target>/<protocol>/<backend>/<vg>/<lv>.
func lvKernelDevice(t *testing.T, container, namespace, claim string) string {
	t.Helper()
	handle := pvVolumeHandle(t, namespace, claim)
	parts := strings.SplitN(handle, "/", 4)
	if len(parts) != 4 || !strings.Contains(parts[3], "/") {
		t.Fatalf("volume handle %q of claim %s/%s is not <target>/<protocol>/<backend>/<vg>/<lv>",
			handle, namespace, claim)
	}
	output := dockerExec(t, container, "lvs", "--noheadings", "-o", "lv_kernel_major,lv_kernel_minor", parts[3])
	fields := strings.Fields(output)
	if len(fields) != 2 {
		t.Fatalf("lvs kernel device of %s in %s = %q, want \"<major> <minor>\"", parts[3], container, output)
	}
	// An inactive LV reports -1 -1.
	for _, field := range fields {
		if _, err := strconv.ParseUint(field, 10, 32); err != nil {
			t.Fatalf("lvs kernel device of %s in %s = %q, want an active LV: %v", parts[3], container, output, err)
		}
	}
	return fields[0] + ":" + fields[1]
}

// blockDeviceDiscardedBytes returns the bytes the host block device
// major:minor has completed as discards since it was created.
func blockDeviceDiscardedBytes(t *testing.T, device string) int64 {
	t.Helper()
	path := "/sys/dev/block/" + device + "/stat"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read I/O statistics of block device %s: %v", device, err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) <= blockStatDiscardSectorsField {
		t.Fatalf("%s has %d fields, want discard statistics (field %d): %q",
			path, len(fields), blockStatDiscardSectorsField+1, raw)
	}
	sectors, err := strconv.ParseInt(fields[blockStatDiscardSectorsField], 10, 64)
	if err != nil {
		t.Fatalf("parse discarded sectors of block device %s from %q: %v", device, raw, err)
	}
	return sectors * blockStatSectorBytes
}
