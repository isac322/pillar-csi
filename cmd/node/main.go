/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package main is the entry point for the pillar-csi node plugin.
// It serves the CSI Identity and Node gRPC services on a Unix domain socket
// so that the Kubernetes CO (kubelet) can invoke NodeStageVolume,
// NodePublishVolume, and related RPCs on every storage-consumer node.
//
// The node plugin runs as a DaemonSet on every worker node.  It does NOT need
// access to the Kubernetes API server at runtime — all volume context is
// forwarded from the controller by the CO via the CSI protocol itself.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr/funcr"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	healthsrv "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	csi "github.com/container-storage-interface/spec/lib/go/csi"

	csisvc "github.com/isac322/pillar-csi/internal/csi"
	"github.com/isac322/pillar-csi/internal/iscsi"
	"github.com/isac322/pillar-csi/internal/runtimepaths"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

// driverName is the CSI provisioner name declared in the StorageClass.
// It must match the name served by the controller plugin.
const driverName = "pillar-csi.bhyoo.com"

const defaultNodeStateDir = "/var/lib/pillar-csi/node"

const defaultNodeShutdownGracePeriod = 5 * time.Second

// iscsiOwnedTargetPrefix is the IQN prefix of every target pillar-csi agents
// export.  The in-process initiator recovers and adopts existing sessions to
// such targets after a pillar-node restart.
const iscsiOwnedTargetPrefix = "iqn.2026-01.com.bhyoo.pillar-csi:"

// ─────────────────────────────────────────────────────────────────────────────
// fabricsConnector — kernel-native NVMe-oF TCP protocol handler
// ─────────────────────────────────────────────────────────────────────────────

// fabricsConnector implements the csisvc.ProtocolHandler interface for the
// NVMe-oF TCP transport using the Linux /dev/nvme-fabrics kernel character
// device.  Unlike NVMeoFConnector it does NOT require nvme-cli to be installed
// in the container image — it speaks to the kernel NVMe-fabrics driver
// directly via the text-based write interface available since Linux 4.15.
//
// Attach writes a comma-separated key=value option string to /dev/nvme-fabrics
// and polls sysfs until the block device node appears.
//
// Detach removes each controller for the given subsystem NQN by writing
// "1" to its delete_controller sysfs entry and waits (bounded) until each
// controller has left sysfs, so an immediate re-stage never races a dying
// controller.
//
// Rescan writes "1" to rescan_controller sysfs entries so the device
// re-reads its capacity after an online volume expansion.
//
// The three sysfs scan methods (nvmeGetDevicePath, nvmeGetDevicePath­ViaController,
// getDevicePathViaNvmeCli) are retained as private helpers.
type fabricsConnector struct {
	// sysfsRoot is the root of the sysfs virtual filesystem.
	// Production value: "/sys".
	sysfsRoot string

	// fabricsDev is the path to the NVMe-fabrics character device.
	// Production value: csisvc.NvmeFabricsDevice.
	fabricsDev string

	// hostNQN is the NVMe-oF host NQN this node identifies as during
	// nvme-fabrics connect.  It MUST equal the NQN published on the
	// CSINode annotation (and therefore the one the controller whitelists
	// via agent.AllowInitiator), otherwise the target rejects the connect
	// with EIO when ACL enforcement is enabled (the default).  Sourced
	// from csisvc.ReadHostNQN at startup and threaded through every
	// nvme-fabrics write as the explicit `hostnqn=` option, because the
	// kernel does not read /etc/nvme/hostnqn itself — only userland
	// nvme-cli does that, and pillar-node writes /dev/nvme-fabrics
	// directly.
	hostNQN string

	// hostID is the NVMe host ID UUID written as the `hostid=` option on
	// every fabrics connect.  Recent Linux kernels reject the write with
	// EINVAL when `hostnqn=` is set without a matching `hostid=`, so the
	// two fields must always travel together.  Persisted to
	// /etc/nvme/hostid alongside the host NQN.
	hostID string

	// removalWait bounds the Detach wait for deleted controllers to leave
	// sysfs.  The zero value selects csisvc.DefaultControllerRemovalWait.
	removalWait csisvc.ControllerRemovalWait

	// readMDTS reads a controller's advertised MDTS for
	// csisvc.LimitNVMeoFTransferSize.  Production value:
	// csisvc.ReadNVMeControllerMDTS.
	readMDTS csisvc.NVMeMDTSReader

	// devDir is where namespace block-device nodes are ensured and mknod
	// creates them.  Empty values select "/dev" and syscall.Mknod; tests
	// point them at a temp dir and a fake so no real node is touched.
	devDir string
	mknod  func(path string, mode uint32, dev int) error
}

// ensureDevNode ensures the device node of block device name matches the
// sysfs dev file (see mknodFromSysfsDev) and returns its path.
func (c *fabricsConnector) ensureDevNode(name, devFile string) (string, error) {
	devPath := "/dev/" + name
	if c.devDir != "" {
		devPath = filepath.Join(c.devDir, name)
	}
	if c.mknod == nil {
		return mknodFromSysfsDev(devPath, devFile)
	}
	return mknodFromSysfsDevWith(devPath, devFile, c.mknod)
}

// newFabricsConnector returns a production-ready fabricsConnector that uses
// /sys as the sysfs root and /dev/nvme-fabrics for connection requests.
func newFabricsConnector(hostNQN, hostID string) *fabricsConnector {
	return &fabricsConnector{
		sysfsRoot:  nodeSysfsRoot,
		fabricsDev: csisvc.NvmeFabricsDevice,
		hostNQN:    hostNQN,
		hostID:     hostID,
		readMDTS:   csisvc.ReadNVMeControllerMDTS,
	}
}

func resolvedDefaultStateDir() string {
	return runtimepaths.ResolveNodeStateDir(defaultNodeStateDir)
}

func nodeReadyFn(fabricsDevice, stateDir string) func(context.Context) (bool, error) {
	return func(_ context.Context) (bool, error) {
		if !pathExists(fabricsDevice) {
			return false, nil
		}
		if !stateDirWritable(stateDir) {
			return false, nil
		}
		return true, nil
	}
}

func pathExists(path string) bool {
	_, statErr := os.Stat(path)
	return statErr == nil
}

func stateDirWritable(stateDir string) bool {
	if stateDir == "" {
		return false
	}
	// MkdirAll is intentional: the chart's DirectoryOrCreate hostPath
	// creates the default directory, but other deployments and the E2E
	// suite workspace may not, and a fresh node would otherwise report
	// Ready=false forever because os.WriteFile cannot create through a
	// missing parent.  Existing mode is preserved; new directories get 0o750.
	mkErr := os.MkdirAll(stateDir, 0o750)
	if mkErr != nil {
		return false
	}
	probeFile := filepath.Join(stateDir, ".probe")
	writeErr := os.WriteFile(probeFile, []byte("ok"), 0o600)
	if writeErr != nil {
		return false
	}
	removeErr := os.Remove(probeFile)
	return removeErr == nil || os.IsNotExist(removeErr)
}

// nvmeConnect establishes an NVMe-oF TCP connection to the given subsystem NQN
// at the given transport address (trAddr) and service ID (TCP port, trSvcID).
//
// It is idempotent: if the subsystem NQN is already connected (detected by
// scanning /sys/class/nvme-subsystem/ or via nvme-cli) the method returns nil
// immediately.
//
// On a new connection it opens /dev/nvme-fabrics and writes:
//
//	transport=tcp,traddr=<trAddr>,trsvcid=<trSvcID>,nqn=<subsysNQN>,
//	    hostnqn=<c.hostNQN>,hostid=<c.hostID>[,ctrl_loss_tmo=N][,reconnect_delay=N][,queue_size=N]
//
// ctrl_loss_tmo / reconnect_delay / queue_size are appended only when the
// VolumeContext carries them; otherwise the kernel defaults apply.
//
// Both identity fields are mandatory:
//
//   - `hostnqn=` whenever the target enforces ACLs (the pillar-csi default
//     with `attr_allow_any_host=0`); the kernel /dev/nvme-fabrics interface
//     does not consult /etc/nvme/hostnqn on its own — only nvme-cli does —
//     so omitting it makes the target reject the connect with EIO.
//   - `hostid=` because recent Linux kernels (~6.x) reject writes that set
//     `hostnqn=` without a matching `hostid=` UUID with EINVAL at parse
//     time, before any TCP attempt; nvme-cli always sends both for the
//     same reason.
//
// The kernel nvme_fabrics module parses the string, creates the controller,
// and initiates the TCP connection synchronously.  Write returns an error if
// the connection fails (target unreachable, invalid NQN, etc.).
// Assembles the nvme-fabrics option string written to the kernel device
// to drive a connect. Both hostnqn and hostid are mandatory together
// because ACL-enforcing targets reject a missing hostnqn with EIO, and
// recent Linux kernels reject a hostnqn without a matching hostid UUID
// with EINVAL at parse time. Kept as a standalone function so the format
// can be regression-tested without opening the kernel device.
func buildFabricsConnectOpts(
	trAddr, trSvcID, subsysNQN, hostNQN, hostID string,
	connectOpts csisvc.NVMeoFConnectOptions,
) string {
	return connectOpts.AppendTo(fmt.Sprintf(
		"transport=tcp,traddr=%s,trsvcid=%s,nqn=%s,hostnqn=%s,hostid=%s",
		trAddr, trSvcID, subsysNQN, hostNQN, hostID))
}

// nvmeConnect runs inside the SP10 pillar_csi.node.nvmeof_connect span.  The
// fabrics option string carries the host NQN and host ID and is never
// recorded on it.
func (c *fabricsConnector) nvmeConnect(
	ctx context.Context,
	subsysNQN, trAddr, trSvcID string,
	connectOpts csisvc.NVMeoFConnectOptions,
) (err error) {
	attrs := []attribute.KeyValue{
		semconv.ServerAddress(trAddr),
		telemetry.KeyNVMeSubsystemNQN.String(subsysNQN),
	}
	port, portErr := strconv.Atoi(trSvcID)
	if portErr == nil {
		attrs = append(attrs, semconv.ServerPort(port))
	}
	ctx, span := telemetry.Tracer().Start(ctx, telemetry.SpanNodeNVMeoFConnect,
		trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	defer func() {
		telemetry.SetSpanError(span, err, "")
		span.End()
	}()

	already, err := c.isConnected(ctx, subsysNQN)
	if err != nil {
		return fmt.Errorf("fabricsConnector nvmeConnect: check existing connection for %q: %w", subsysNQN, err)
	}
	span.SetAttributes(telemetry.KeyNVMeAlreadyConnected.Bool(already))
	if already {
		return nil
	}
	// A controller of this NQN that the kernel is still deleting (its own
	// ctrl_loss_tmo teardown, not our unstage) must be gone before a new
	// connection, or device discovery can return its dying namespace.
	dyingStart := time.Now()
	err = csisvc.WaitForDyingControllers(ctx, c.sysfsRoot, subsysNQN, c.removalWait)
	span.SetAttributes(telemetry.KeyNVMeDyingWaitDuration.Float64(time.Since(dyingStart).Seconds()))
	if err != nil {
		return fmt.Errorf("fabricsConnector nvmeConnect: %w", err)
	}

	f, err := os.OpenFile(c.fabricsDev, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("fabricsConnector nvmeConnect: open %s: %w", c.fabricsDev, err)
	}
	defer func() { _ = f.Close() }() //nolint:errcheck

	// Write the connection parameters as a comma-separated key=value string.
	// The kernel nvme_fabrics driver parses this in nvmf_dev_write() and
	// initiates the TCP connection via nvmf_create_ctrl().  Build the
	// string through buildFabricsConnectOpts so the format is unit-testable
	// without opening /dev/nvme-fabrics.
	opts := buildFabricsConnectOpts(trAddr, trSvcID, subsysNQN, c.hostNQN, c.hostID, connectOpts)
	_, err = fmt.Fprintf(f, "%s\n", opts)
	if err != nil {
		return fmt.Errorf("fabricsConnector nvmeConnect: write to %s (nqn=%s): %w",
			c.fabricsDev, subsysNQN, err)
	}
	return nil
}

// forEachNVMeController scans /sys/class/nvme-subsystem/ for the given NQN
// and invokes fn for each NVMe controller name (nvmeX, not nvmeXnY) found
// under that subsystem.  Returns nil when the subsystem is not present.
func (c *fabricsConnector) forEachNVMeController(subsysNQN string, fn func(ctrlName string) error) error {
	subsysDir := filepath.Join(c.sysfsRoot, "class", "nvme-subsystem")

	entries, err := os.ReadDir(subsysDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("fabricsConnector: read %s: %w", subsysDir, err)
	}

	var errs []error
	for _, entry := range entries {
		matches, readErr := csisvc.SubsystemMatchesNQN(subsysDir, entry.Name(), subsysNQN)
		if readErr != nil {
			errs = append(errs, readErr)
			continue
		}
		if !matches {
			continue
		}
		subsysPath := filepath.Join(subsysDir, entry.Name())
		ctrlEntries, readErr := os.ReadDir(subsysPath)
		if readErr != nil {
			errs = append(errs, fmt.Errorf("read subsystem dir %s: %w", subsysPath, readErr))
			continue
		}
		for _, ctrlEntry := range ctrlEntries {
			if !csisvc.IsNVMeControllerEntry(ctrlEntry.Name()) {
				continue
			}
			fnErr := fn(ctrlEntry.Name())
			if fnErr != nil {
				errs = append(errs, fnErr)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("fabricsConnector: %w", errors.Join(errs...))
	}
	return nil
}

// nvmeDisconnect tears down all NVMe-oF controllers associated with the given
// subsystem NQN and returns only after each one has left sysfs; a controller
// still present when the bounded wait expires is an error (see
// csisvc.DeleteSubsystemControllers for why the write alone is not enough).
//
// It is idempotent: if the NQN is not connected the method returns nil
// immediately.
//
// It runs inside the SP12 pillar_csi.node.nvmeof_disconnect span.
func (c *fabricsConnector) nvmeDisconnect(ctx context.Context, subsysNQN string) error {
	ctx, span := telemetry.Tracer().Start(ctx, telemetry.SpanNodeNVMeoFDisconnect,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(telemetry.KeyNVMeSubsystemNQN.String(subsysNQN)))
	defer span.End()
	err := csisvc.DisconnectSubsystem(ctx, c.sysfsRoot, subsysNQN, c.removalWait)
	telemetry.SetSpanError(span, err, "")
	return err
}

// nvmeGetDevicePath returns the /dev/nvmeXnY block-device path for the given
// subsystem NQN after a successful nvmeConnect call.
//
// Strategy:
//  1. Fast path: scan /sys/class/nvme-subsystem/ for the matching NQN and
//     look for namespace entries (nvmeXnY) directly in the subsystem sysfs
//     directory.  On kernels that expose namespace symlinks there this is
//     O(1) and does not require any child-process execution.
//  2. Fallback: scan /dev/nvme*n* and identify each device's NQN via
//     "nvme id-ctrl -o json".  This path is taken when:
//     (a) /sys/class/nvme-subsystem/ is not readable (containerised sysfs
//     restrictions), OR
//     (b) the NQN was found in sysfs but no namespace entry appeared
//     directly in the subsystem directory (some kernel versions place
//     namespace sysfs entries as children of the controller device, not
//     as direct children of the subsystem class directory).
//
// Returns ("", nil) when the device is not yet visible; callers
// should poll until a non-empty path is returned or a deadline is exceeded.
//
//nolint:gocognit // primary scan + fallback paths kept inline for locality
func (c *fabricsConnector) nvmeGetDevicePath(ctx context.Context, subsysNQN string) (string, error) {
	// ── Primary path: sysfs nvme-subsystem scan ──────────────────────────────
	subsysDir := filepath.Join(c.sysfsRoot, "class", "nvme-subsystem")
	entries, err := os.ReadDir(subsysDir)
	if err == nil {
		nqnFound := false
		for _, entry := range entries {
			subsysPath := filepath.Join(subsysDir, entry.Name())
			nqnFile := filepath.Join(subsysPath, "subsysnqn")
			nqnBytes, readErr := os.ReadFile(nqnFile) //nolint:gosec
			if readErr != nil || strings.TrimSpace(string(nqnBytes)) != subsysNQN {
				continue
			}
			nqnFound = true
			// Found the matching subsystem NQN.  Scan for namespace block-device
			// entries (nvmeXnY) directly in this subsystem sysfs directory.
			// On kernels that do NOT expose namespaces here (they are children
			// of the controller device), nsEntries will contain no matching
			// entry and we break to fall through to the nvme-cli path.
			nsEntries, readErr := os.ReadDir(subsysPath)
			if readErr != nil {
				break // can't enumerate namespace entries; fall through to nvme-cli
			}
			namespaceSeen := false
			for _, nsEntry := range nsEntries {
				name := nsEntry.Name()
				// Filter for namespace block-device names (nvmeXnY).
				// Controller entries are nvmeX (no 'n' after digits).
				suffix := strings.TrimPrefix(name, "nvme")
				if suffix == name {
					continue // does not start with "nvme"
				}
				nIdx := strings.IndexRune(suffix, 'n')
				if nIdx < 0 {
					continue // no 'n' separator → controller entry (nvmeX), skip
				}
				afterN := suffix[nIdx+1:]
				if strings.ContainsAny(afterN, "p") {
					continue // partition entry (nvmeXnYpZ), skip
				}
				namespaceSeen = true
				// Always validate the existing node against the live sysfs
				// dev_t. A prior disconnect can leave /dev/nvmeXnY behind
				// while the kernel reuses the name with a new minor.
				devFile := filepath.Join(subsysPath, name, "dev")
				dp, mkErr := c.ensureDevNode(name, devFile)
				if mkErr != nil {
					fmt.Fprintf(os.Stderr,
						"pillar-node: nvmeGetDevicePath: ensure %s from %s: %v\n",
						name, devFile, mkErr)
					continue
				}
				return dp, nil
			}
			// The NQN was found in sysfs but no namespace entry appeared
			// directly in the subsystem directory.  This happens on kernel
			// versions that place namespace sysfs entries as children of the
			// controller device (/sys/class/nvme/nvmeX/nvmeXnY/) rather than
			// as direct entries of the subsystem class directory.
			// Try the controller-based sysfs path with auto-mknod before
			// falling through to the slower nvme-cli scan.
			dp, ctrlErr := c.getDevicePathViaController(subsysPath)
			if ctrlErr == nil && dp != "" {
				return dp, nil
			}
			if namespaceSeen {
				// A namespace exists in the primary layout, but ensuring its
				// device node failed and was logged above. Keep polling instead
				// of misreporting a layout mismatch and scanning stale devices.
				return "", nil
			}
			fmt.Fprintf(os.Stderr,
				"pillar-node: nvmeGetDevicePath: NQN %q found in sysfs but no "+
					"namespace in subsystem dir %s; falling back to nvme-cli\n",
				subsysNQN, subsysPath)
			break
		}
		if !nqnFound {
			// The subsystem NQN was not found in sysfs at all.  This means
			// nvmeConnect() has not yet completed (or the subsystem is not yet
			// visible to this container).  Return "" to keep polling; do NOT
			// fall through to nvme-cli because the device cannot exist yet.
			return "", nil
		}
		// nqnFound == true but no namespace device path was returned above.
		// Fall through to nvme-cli to discover the device via direct query.
	}

	// ── Fallback: nvme-cli scan ──────────────────────────────────────────────
	// Either /sys/class/nvme-subsystem/ is not readable, or the NQN was
	// found in sysfs but no namespace entry appeared in the subsystem
	// directory (kernel version difference).  Scan /dev/nvme*n* and query
	// each device's subsystem NQN via "nvme id-ctrl -o json".
	return c.getDevicePathViaNvmeCli(ctx, subsysNQN)
}

// getDevicePathViaController discovers NVMe namespace block devices by scanning
// controller entries in the given subsystem sysfs directory, then resolving
// the corresponding namespace entries via /sys/class/nvme/<ctrl>/<ctrl>nY/.
//
// When the namespace device node is absent from /dev/ (common in containers
// that mount only a filtered /dev), the method reads the major:minor numbers
// from the sysfs "dev" file and calls syscall.Mknod to create the node.
//
// The subsysPath argument is the absolute path to the nvme-subsystem class directory for
// the matching NQN, e.g. /sys/class/nvme-subsystem/nvme-subsys2.
func (c *fabricsConnector) getDevicePathViaController(subsysPath string) (string, error) {
	ctrlEntries, err := os.ReadDir(subsysPath)
	if err != nil {
		return "", fmt.Errorf("readdir %s: %w", subsysPath, err)
	}
	for _, ctrlEntry := range ctrlEntries {
		name := ctrlEntry.Name()
		if !strings.HasPrefix(name, "nvme") {
			continue
		}
		suffix := strings.TrimPrefix(name, "nvme")
		// Skip namespace entries (nvmeXnY contain 'n'); only process controller entries (nvmeX).
		if strings.ContainsRune(suffix, 'n') {
			continue
		}
		// Found controller nvmeX.  Look for namespace devices at
		// /sys/class/nvme/nvmeX/nvmeXnY/.
		ctrlSysPath := filepath.Join(c.sysfsRoot, "class", "nvme", name)
		nsEntries, nsErr := os.ReadDir(ctrlSysPath)
		if nsErr != nil {
			continue
		}
		for _, nsEntry := range nsEntries {
			nsName := nsEntry.Name()
			// Namespace names must start with <ctrl>n (e.g. nvme2n1).
			prefix := name + "n"
			if !strings.HasPrefix(nsName, prefix) {
				continue
			}
			afterN := strings.TrimPrefix(nsName, prefix)
			if afterN == "" || strings.ContainsAny(afterN, "p") {
				continue // empty or partition
			}
			devFile := filepath.Join(ctrlSysPath, nsName, "dev")
			dp, mkErr := c.ensureDevNode(nsName, devFile)
			if mkErr != nil {
				fmt.Fprintf(os.Stderr,
					"pillar-node: getDevicePathViaController: ensure %s from %s: %v\n",
					nsName, devFile, mkErr)
				continue
			}
			return dp, nil
		}
	}
	return "", nil
}

// mknodFromSysfsDev reads "<major>:<minor>" from the sysfs file at devFile,
// ensures devPath represents that exact block device, and returns devPath.
// Unreadable or malformed sysfs data and device-node replacement failures are
// returned as errors so callers can log the cause and continue polling.
//
// Stale-node recovery: when devPath already exists, the existing major:minor
// is compared against the sysfs value.  If they match, the node is reused
// (idempotency for concurrent attempts and pre-existing udev-created nodes).
// If they differ (the controller disconnected and the kernel re-issued a
// new minor on reconnect), the stale node is unlinked and recreated so a
// subsequent open(2) does not hit ENXIO on the prior minor.
//
// This is the shared mknod path used by both the primary subsystem-class scan
// (nvmeGetDevicePath) and the controller-class fallback
// (getDevicePathViaController); without it containerised hosts that lack udev
// (Kind, distroless) would never see /dev/nvmeXnY appear and every
// NodeStageVolume would hit the 30s attach timeout.
func mknodFromSysfsDev(devPath, devFile string) (string, error) {
	return mknodFromSysfsDevWith(devPath, devFile, syscall.Mknod)
}

func mknodFromSysfsDevWith(
	devPath, devFile string,
	mknod func(path string, mode uint32, dev int) error,
) (string, error) {
	devBytes, readErr := os.ReadFile(devFile) //nolint:gosec // sysfs path under /sys/class/nvme*
	if readErr != nil {
		return "", readErr
	}
	parts := strings.SplitN(strings.TrimSpace(string(devBytes)), ":", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("mknodFromSysfsDev: malformed dev %q in %s", string(devBytes), devFile)
	}
	major, majErr := strconv.ParseUint(parts[0], 10, 32)
	if majErr != nil {
		return "", fmt.Errorf("mknodFromSysfsDev: parse major in %q: %w", string(devBytes), majErr)
	}
	minor, minErr := strconv.ParseUint(parts[1], 10, 32)
	if minErr != nil {
		return "", fmt.Errorf("mknodFromSysfsDev: parse minor in %q: %w", string(devBytes), minErr)
	}
	// Linux makedev bit-packing:
	//   minor bits 0-7   → device bits 0-7
	//   major bits 0-11  → device bits 8-19
	//   minor bits 8-19  → device bits 20-31
	//   major bits 12+   → device bits 32+
	dev := int((minor & 0xff) | ((major & 0xfff) << 8) | //nolint:gosec // G115: makedev bit-packing
		((minor &^ 0xff) << 12) | ((major &^ 0xfff) << 32))

	// Drop any pre-existing node whose major:minor does not match the
	// current sysfs value.  The most common cause is a previous NodeStage
	// cycle that left /dev/nvmeXnY behind; after detach + reconnect the
	// kernel assigns a fresh minor and the stale node would resolve to a
	// nonexistent kernel block device (open returns ENXIO).
	st, statErr := os.Stat(devPath)
	if statErr == nil {
		sysSt, ok := st.Sys().(*syscall.Stat_t)
		//nolint:gosec,nolintlint // G115 only applies on Linux, where dev_t safely fits int.
		if ok && int(sysSt.Rdev) != dev {
			rmErr := os.Remove(devPath)
			if rmErr != nil && !os.IsNotExist(rmErr) {
				return "", fmt.Errorf("mknodFromSysfsDev: remove stale %s: %w", devPath, rmErr)
			}
		}
	}

	mknodErr := mknod(devPath, syscall.S_IFBLK|0o600, dev)
	if mknodErr != nil && !os.IsExist(mknodErr) {
		return "", fmt.Errorf("mknodFromSysfsDev: mknod %s (%d:%d): %w", devPath, major, minor, mknodErr)
	}
	fmt.Fprintf(os.Stderr,
		"pillar-node: mknodFromSysfsDev: ensured %s (%d:%d)\n", devPath, major, minor)
	return devPath, nil
}

// getDevicePathViaNvmeCli scans /dev/nvme*n* and uses "nvme id-ctrl -o json"
// to identify the device matching subsysNQN.  This fallback is used in
// containerized environments where /sys/class/nvme-subsystem/ is unavailable
// or does not expose namespace entries directly in the subsystem class dir.
func (c *fabricsConnector) getDevicePathViaNvmeCli(ctx context.Context, subsysNQN string) (string, error) {
	devEntries, err := os.ReadDir("/dev")
	if err != nil {
		return "", nil //nolint:nilerr // /dev unreadable; treat as not found
	}
	for _, entry := range devEntries {
		name := entry.Name()
		if !strings.HasPrefix(name, "nvme") {
			continue
		}
		suffix := strings.TrimPrefix(name, "nvme")
		// Must have namespace separator 'n': nvme0n1 yes, nvme0 no.
		nIdx := strings.IndexRune(suffix, 'n')
		if nIdx < 0 {
			continue
		}
		// Exclude partitions: nvme0n1p1 has 'p' after the namespace number.
		afterN := suffix[nIdx+1:]
		if strings.ContainsAny(afterN, "p") {
			continue
		}
		devPath := "/dev/" + name
		// Verify the device node exists as a block device before probing.
		info, statErr := os.Stat(devPath)
		if statErr != nil || info.Mode()&os.ModeDevice == 0 {
			continue
		}
		// Query the subsystem NQN of this device via nvme id-ctrl.
		nqn, nqnErr := c.nvmeIDCtrlSubNQN(ctx, devPath)
		if nqnErr != nil {
			fmt.Fprintf(os.Stderr,
				"pillar-node: nvme-cli: id-ctrl %s failed: %v\n", devPath, nqnErr)
			continue
		}
		if strings.TrimSpace(nqn) != subsysNQN {
			continue
		}
		fmt.Fprintf(os.Stderr,
			"pillar-node: nvme-cli: found device %s for NQN %q\n", devPath, subsysNQN)
		return devPath, nil
	}
	return "", nil
}

// nvmeIDCtrlSubNQN runs "nvme id-ctrl -o json <devPath>" and returns the
// subnqn field.  Returns ("", err) on any failure.
func (*fabricsConnector) nvmeIDCtrlSubNQN(ctx context.Context, devPath string) (string, error) {
	out, err := exec.CommandContext(ctx, "nvme", "id-ctrl", "-o", "json", devPath).Output() //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("nvme id-ctrl %s: %w", devPath, err)
	}
	var info struct {
		Subnqn string `json:"subnqn"`
	}
	jsonErr := json.Unmarshal(out, &info)
	if jsonErr != nil {
		return "", fmt.Errorf("parse nvme id-ctrl output for %s: %w", devPath, jsonErr)
	}
	return info.Subnqn, nil
}

// isConnected returns true when the given NQN has an active NVMe-oF connection
// visible via sysfs or nvme-cli.
//
// It tries /sys/class/nvme-subsystem/ first (fast path), then falls back to
// scanning /dev/nvme*n* with nvme id-ctrl (for containerized environments
// where the sysfs nvme-subsystem class is restricted by network namespace).
// A matching subsystem only counts when csisvc.SubsystemHasActiveController
// reports a live or reconnecting controller; the empty subsystem left behind
// after ctrl_loss_tmo removes every controller is treated as disconnected.
func (c *fabricsConnector) isConnected(ctx context.Context, subsysNQN string) (bool, error) {
	// ── Primary: sysfs scan ──────────────────────────────────────────────────
	subsysDir := filepath.Join(c.sysfsRoot, "class", "nvme-subsystem")
	entries, err := os.ReadDir(subsysDir)
	if err == nil {
		for _, entry := range entries {
			nqnFile := filepath.Join(subsysDir, entry.Name(), "subsysnqn")
			nqnBytes, readErr := os.ReadFile(nqnFile) //nolint:gosec
			if readErr != nil {
				continue
			}
			if strings.TrimSpace(string(nqnBytes)) != subsysNQN {
				continue
			}
			active, activeErr := csisvc.SubsystemHasActiveController(filepath.Join(subsysDir, entry.Name()))
			if activeErr != nil {
				return false, fmt.Errorf("fabricsConnector isConnected: %w", activeErr)
			}
			if active {
				return true, nil
			}
		}
		// Sysfs is readable; no active controller for the NQN → not connected.
		return false, nil
	}

	// ── Fallback: nvme-cli scan ──────────────────────────────────────────────
	// Sysfs nvme-subsystem is unavailable; scan /dev/nvme*n* instead.
	path, nvmeErr := c.getDevicePathViaNvmeCli(ctx, subsysNQN)
	if nvmeErr != nil {
		return false, nil //nolint:nilerr // can't determine; assume not connected
	}
	return path != "", nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ProtocolHandler interface implementation
// ─────────────────────────────────────────────────────────────────────────────

// Compile-time assertion that fabricsConnector satisfies ProtocolHandler
// and NVMeoFTransferLimiter.
var (
	_ csisvc.ProtocolHandler       = (*fabricsConnector)(nil)
	_ csisvc.NVMeoFTransferLimiter = (*fabricsConnector)(nil)
)

// TransferLimitSysfs returns the sysfs root and the MDTS reader Attach
// passes to csisvc.LimitNVMeoFTransferSize.
func (c *fabricsConnector) TransferLimitSysfs() (sysfsRoot string, readMDTS csisvc.NVMeMDTSReader) {
	return c.sysfsRoot, c.readMDTS
}

// nvmeAttachTimeout is the maximum time Attach waits for the NVMe block
// device to appear in /dev after a successful nvmeConnect call.
const nvmeAttachTimeout = 30 * time.Second

// nvmeAttachPollInterval is the sleep between successive device-path polls.
const nvmeAttachPollInterval = 500 * time.Millisecond

// Attach establishes the NVMe-oF TCP connection and waits for the block
// device to appear.  It returns an AttachResult with the DevicePath set
// to the /dev/nvmeXnY path and a NVMeoFStageState suitable for Detach/Rescan.
//
// Attach is idempotent: if the subsystem NQN is already connected the
// nvmeConnect step is a no-op and the polling resolves immediately.
//
// Once the device is present — on a fresh connect and on an existing
// connection alike — Attach caps the namespace devices' max_sectors_kb to
// the volume's max data transfer size when the target advertises no MDTS
// (csisvc.LimitNVMeoFTransferSize); a failure fails the Attach.  The size
// is parsed with the fabrics tuning before connecting, so a malformed
// VolumeContext never connects.
func (c *fabricsConnector) Attach(ctx context.Context, params csisvc.AttachParams) (*csisvc.AttachResult, error) {
	subsysNQN := params.ConnectionID
	trAddr := params.Address
	trSvcID := params.Port

	connectOpts, optsErr := csisvc.ParseNVMeoFConnectOptions(params.Extra)
	if optsErr != nil {
		return nil, fmt.Errorf("fabricsConnector Attach: %w", optsErr)
	}
	maxTransferSize, sizeErr := csisvc.ParseNVMeoFMaxDataTransferSize(params.Extra)
	if sizeErr != nil {
		return nil, fmt.Errorf("fabricsConnector Attach: %w", sizeErr)
	}

	// Step 1: establish the NVMe-oF TCP connection (idempotent).
	connectErr := c.nvmeConnect(ctx, subsysNQN, trAddr, trSvcID, connectOpts)
	if connectErr != nil {
		return nil, fmt.Errorf("fabricsConnector Attach: connect to %q at %s:%s: %w",
			subsysNQN, trAddr, trSvcID, connectErr)
	}

	// Step 2: poll until the block-device node appears in /dev.
	devPath, waitErr := c.waitForDevice(ctx, subsysNQN)
	if waitErr != nil {
		return nil, waitErr
	}

	// Step 3: limit the request size when the target advertises no MDTS.
	limitErr := csisvc.LimitNVMeoFTransferSize(c.sysfsRoot, subsysNQN, maxTransferSize, c.readMDTS)
	if limitErr != nil {
		return nil, fmt.Errorf("fabricsConnector Attach: limit transfer size for %q: %w", subsysNQN, limitErr)
	}
	return &csisvc.AttachResult{
		DevicePath: devPath,
		State: &csisvc.NVMeoFProtocolState{
			SubsysNQN: subsysNQN,
			Address:   trAddr,
			Port:      trSvcID,
		},
	}, nil
}

// waitForDevice polls, bounded by nvmeAttachTimeout, until the block device
// of subsysNQN appears and returns its path.  It runs inside the SP11
// pillar_csi.node.nvmeof_device_wait span, which records the number of
// polls and error.type=timeout when the bound expires.
func (c *fabricsConnector) waitForDevice(ctx context.Context, subsysNQN string) (devPath string, err error) {
	ctx, span := telemetry.Tracer().Start(ctx, telemetry.SpanNodeNVMeoFDeviceWait,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(telemetry.KeyNVMeSubsystemNQN.String(subsysNQN)))
	attempts := 0
	errorType := ""
	defer func() {
		span.SetAttributes(telemetry.KeyPollAttempts.Int(attempts))
		telemetry.SetSpanError(span, err, errorType)
		span.End()
	}()

	pollCtx, cancel := context.WithTimeout(ctx, nvmeAttachTimeout)
	defer cancel()

	for {
		attempts++
		path, devErr := c.nvmeGetDevicePath(pollCtx, subsysNQN)
		if devErr != nil {
			return "", fmt.Errorf("fabricsConnector Attach: get device path for %q: %w",
				subsysNQN, devErr)
		}
		if path != "" {
			return path, nil
		}
		select {
		case <-pollCtx.Done():
			errorType = telemetry.ErrorTypeTimeout
			return "", fmt.Errorf("fabricsConnector Attach: block device for NQN %q "+
				"did not appear within %s", subsysNQN, nvmeAttachTimeout)
		case <-time.After(nvmeAttachPollInterval):
			// next iteration
		}
	}
}

// Detach tears down the NVMe-oF TCP connection identified by the persisted
// ProtocolState.  The state must be a *csisvc.NVMeoFProtocolState.
//
// Detach is idempotent: if the NQN is not currently connected the call
// returns nil immediately.
func (c *fabricsConnector) Detach(ctx context.Context, state csisvc.ProtocolState) error {
	nvmeoFState, ok := state.(*csisvc.NVMeoFProtocolState)
	if !ok {
		return fmt.Errorf("fabricsConnector Detach: expected *NVMeoFProtocolState, got %T", state)
	}
	return c.nvmeDisconnect(ctx, nvmeoFState.SubsysNQN)
}

// Rescan triggers a controller rescan on the NVMe subsystem so that the
// block device re-reads its capacity after an online volume expansion.
//
// It writes "1" to the rescan_controller sysfs entry for every NVMe
// controller associated with the given subsystem NQN.  If the sysfs path
// does not exist (e.g. in a test environment) Rescan is a no-op.
func (c *fabricsConnector) Rescan(_ context.Context, state csisvc.ProtocolState) error {
	nvmeoFState, ok := state.(*csisvc.NVMeoFProtocolState)
	if !ok {
		return fmt.Errorf("fabricsConnector Rescan: expected *NVMeoFProtocolState, got %T", state)
	}
	return c.nvmeRescan(nvmeoFState.SubsysNQN)
}

// nvmeRescan writes "1" to the rescan_controller sysfs file for every NVMe
// controller under the given subsystem NQN.
func (c *fabricsConnector) nvmeRescan(subsysNQN string) error {
	return c.forEachNVMeController(subsysNQN, func(name string) error {
		rescanPath := filepath.Join(c.sysfsRoot, "class", "nvme", name, "rescan_controller")
		writeErr := os.WriteFile(rescanPath, []byte("1"), 0o600)
		if writeErr != nil {
			return fmt.Errorf("rescan controller %s: %w", rescanPath, writeErr)
		}
		return nil
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// mkdirMounter — Mounter wrapper that pre-creates mount-target directories
// ─────────────────────────────────────────────────────────────────────────────

// mkdirMounter wraps a csisvc.Mounter and ensures that the target directory
// exists before FormatAndMount is called.
//
// The CSI spec (§4.7) states that the CO pre-creates staging_target_path
// before calling NodeStageVolume.  In a typical deployment the kubelet
// creates /var/lib/kubelet/plugins/kubernetes.io/csi/<hash>/globalmount on
// the host (Kind node container).  However, the pillar-node DaemonSet only
// bind-mounts /var/lib/kubelet/plugins/pillar-csi.bhyoo.com/ from the host;
// the /var/lib/kubelet/plugins/kubernetes.io/csi/ subtree is not mounted into
// the node plugin container.  FormatAndMount therefore fails with "mount
// point does not exist" when it tries to call mount(8) on a path that only
// exists on the host but is absent from the container's mount namespace.
//
// Creating the directory inside the container before mounting resolves the
// issue: the format-and-mount proceeds in the container's namespace, and the
// subsequent NodePublishVolume bind-mount from staging → target (which IS
// under /var/lib/kubelet/pods with Bidirectional propagation) then propagates
// the ext4 filesystem to the host, making it visible to the application pod.
type mkdirMounter struct {
	wrapped csisvc.Mounter
}

// FormatAndMount creates the target directory if it does not exist, then
// delegates to the wrapped Mounter's FormatAndMount.
func (m *mkdirMounter) FormatAndMount(
	ctx context.Context, source, target, fsType string, options, formatOptions []string,
) error {
	mkdirErr := os.MkdirAll(target, 0o750)
	if mkdirErr != nil {
		return fmt.Errorf("mkdirMounter: create mount target %q: %w", target, mkdirErr)
	}
	return m.wrapped.FormatAndMount(ctx, source, target, fsType, options, formatOptions)
}

// Mount provisions the target before delegating to the wrapped Mounter.
// Non-bind filesystem mounts need a directory target; their source may be a
// remote export rather than a local path. Bind mounts need a directory target
// for directory sources and a regular-file target for device or file sources.
func (m *mkdirMounter) Mount(source, target, fsType string, options []string) error {
	sourceIsDir := true
	if slices.Contains(options, "bind") {
		st, statErr := os.Stat(source)
		sourceIsDir = statErr == nil && st.IsDir()
	}
	if sourceIsDir {
		mkdirErr := os.MkdirAll(target, 0o750)
		if mkdirErr != nil {
			return fmt.Errorf("mkdirMounter: create mount target %q: %w", target, mkdirErr)
		}
	} else {
		mkParentErr := os.MkdirAll(filepath.Dir(target), 0o750)
		if mkParentErr != nil {
			return fmt.Errorf("mkdirMounter: create parent dir for file-bind target %q: %w", target, mkParentErr)
		}
		// target is a kubelet-managed CSI volumeDevices path under
		// /var/lib/kubelet/plugins/.../volumeDevices/{staging,publish}/.
		//nolint:gosec // G304: kubelet-controlled CSI staging path
		f, createErr := os.OpenFile(target, os.O_RDWR|os.O_CREATE, 0o600)
		if createErr != nil && !os.IsExist(createErr) {
			return fmt.Errorf("mkdirMounter: create file bind-mount target %q: %w", target, createErr)
		}
		if f != nil {
			_ = f.Close() //nolint:errcheck // best-effort close; only used as mount target sentinel
		}
	}
	return m.wrapped.Mount(source, target, fsType, options)
}

// Unmount delegates to the wrapped Mounter unchanged.
func (m *mkdirMounter) Unmount(target string) error {
	return m.wrapped.Unmount(target)
}

// CheckMountReadable delegates to the wrapped Mounter unchanged.
func (m *mkdirMounter) CheckMountReadable(target string) error {
	return m.wrapped.CheckMountReadable(target)
}

// CheckMountHealth delegates to the wrapped Mounter unchanged.
func (m *mkdirMounter) CheckMountHealth(target string) error {
	return m.wrapped.CheckMountHealth(target)
}

// HasOtherMounts delegates to the wrapped Mounter unchanged.
func (m *mkdirMounter) HasOtherMounts(target string) (bool, error) {
	return m.wrapped.HasOtherMounts(target)
}

// MountSource delegates to the wrapped Mounter unchanged.
func (m *mkdirMounter) MountSource(target string) (string, error) {
	return m.wrapped.MountSource(target)
}

// MountEntryExists delegates to the wrapped Mounter unchanged.
func (m *mkdirMounter) MountEntryExists(target string) (bool, error) {
	return m.wrapped.MountEntryExists(target)
}

// ─────────────────────────────────────────────────────────────────────────────
// main
// ─────────────────────────────────────────────────────────────────────────────

func nodeProtocolHandlers(
	hostNQN, hostID string, iscsiInitiator *iscsi.Initiator, iscsiIQN string,
) map[string]csisvc.ProtocolHandler {
	handlers := map[string]csisvc.ProtocolHandler{
		csisvc.ProtocolNVMeoFTCP: newFabricsConnector(hostNQN, hostID),
	}
	if iscsiInitiator != nil {
		handlers[csisvc.ProtocolISCSI] = csisvc.NewISCSIHandler(iscsiInitiator, iscsiIQN)
	}
	if csisvc.NFSClientAvailable() {
		handlers[csisvc.ProtocolNFS] = csisvc.NewNFSHandler()
	}
	return handlers
}

func main() {
	nodeID := flag.String("node-id", "",
		"Unique identifier for this Kubernetes node (typically the Node name). Required.")
	csiSocket := flag.String("csi-socket", "/var/lib/kubelet/plugins/pillar-csi.bhyoo.com/csi.sock",
		"Path to the Unix domain socket on which the CSI gRPC server listens.")
	metricsAddr := flag.String("metrics-bind-address", metricsDisabled,
		"The address the plaintext Prometheus /metrics endpoint binds to, e.g. :9502. "+
			"Leave as 0 to disable the metrics endpoint.")
	iscsiNameFile := flag.String("iscsi-initiator-name-file", csisvc.DefaultISCSIInitiatorNameFile,
		"open-iscsi style file holding this node's iSCSI initiator IQN (InitiatorName=...). "+
			"Generated and persisted on first start when absent.")
	iscsiNetns := flag.String("iscsi-netlink-netns", "",
		"Path to a network namespace file (e.g. /proc/1/ns/net) in which the NETLINK_ISCSI socket is "+
			"opened. Empty uses the pod's own namespace (hostNetwork). Needed only for nested-container nodes.")
	trimInterval := flag.Duration("trim-interval", defaultTrimInterval, trimIntervalUsage)
	flag.Parse()
	validateTrimIntervalOrExit(*trimInterval)

	if *nodeID == "" {
		// Fall back to the NODE_NAME env var injected by the DaemonSet pod spec
		// (fieldRef: spec.nodeName) so operators don't have to pass --node-id explicitly.
		*nodeID = os.Getenv("NODE_NAME")
	}
	if *nodeID == "" {
		fmt.Fprintln(os.Stderr, "error: --node-id (or NODE_NAME env var) is required")
		os.Exit(1)
	}

	// Determine the driver version from build metadata when available.
	version, _ := telemetry.BuildVersion()

	// ── iSCSI initiator ────────────────────────────────────────────────────
	// The in-process initiator needs the kernel iscsi_tcp transport.  When
	// it is absent the iSCSI handler is not registered and the IQN is not
	// published, so iscsi volumes fail NodeStage with an explicit error.
	// ctx lives as long as the gRPC server; canceling it after Serve
	// returns stops the initiator's netlink reader and session-recovery
	// supervisor.
	ctx, cancel := context.WithCancel(context.Background())
	iscsiInitiator, iscsiIQN := startISCSIInitiatorOrExit(ctx, *iscsiNameFile, *iscsiNetns)

	// ── Publish node identity annotations to the CSINode object ──────────
	// Read /etc/nvme/hostnqn and write it as the
	// pillar-csi.bhyoo.com/nvmeof-host-nqn annotation (plus the iSCSI
	// initiator IQN as pillar-csi.bhyoo.com/iscsi-initiator-iqn when the
	// iSCSI initiator is enabled) on the CSINode named after this node.
	// The controller plugin reads these annotations when processing
	// ControllerPublishVolume to resolve the initiator identity without
	// assuming node_id == NQN/IQN (RFC §5.2).
	//
	// Publication is best-effort with a short retry loop: the CSINode object
	// is created by kubelet during driver registration, which may race with
	// this startup path.  If publication fails after retries we log and
	// continue — volume attach will return FailedPrecondition until the
	// annotation is present, which is the expected degraded behavior.
	publishNodeIdentity(*nodeID, iscsiIQN)

	// Resolve the local host NQN now that publishNodeIdentity has read or
	// generated /etc/nvme/hostnqn.  The fabricsConnector must thread this
	// exact value into every nvme-fabrics connect; see the field doc on
	// fabricsConnector.hostNQN for the kernel-vs-userland contract.
	hostNQN, hostID := resolveHostIdentityOrExit()

	// ── Build the CSI service implementations ──────────────────────────────
	// Build the protocol handlers. NVMe-oF is always available; iSCSI and NFS
	// are registered only when their node-side prerequisites are present.
	handlers := nodeProtocolHandlers(hostNQN, hostID, iscsiInitiator, iscsiIQN)
	stateDir := resolvedDefaultStateDir()
	identitySrv := csisvc.NewIdentityServerWithReadyFn(
		driverName,
		version,
		nodeReadyFn(csisvc.NvmeFabricsDevice, stateDir),
	)
	// The exec DeviceMapper (dmsetup) holds the backend device of a local
	// attach on the storage node; see csisvc.DeviceMapper.
	nodeSrv := csisvc.NewNodeServer(*nodeID, handlers, &mkdirMounter{wrapped: csisvc.NewKubeMounter()}).
		WithDeviceMapper(csisvc.NewExecDeviceMapper())
	restoreProtocolSessions(nodeSrv)

	lis := listenCSISocketOrExit(*csiSocket)

	// ── Tracing, metrics, and the gRPC server ─────────────────────────────
	obs := startObservability(*metricsAddr, version)
	stopTrim := startTrimmerOrExit(ctx, nodeSrv, *trimInterval, obs.trim)
	startTransferLimitReconciler(ctx, nodeSrv)
	grpcSrv := newNodeGRPCServer()
	csi.RegisterIdentityServer(grpcSrv, identitySrv)
	csi.RegisterNodeServer(grpcSrv, nodeSrv)
	healthSrv := healthsrv.NewServer()
	healthpb.RegisterHealthServer(grpcSrv, healthSrv)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	// Graceful shutdown on SIGTERM / SIGINT.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		runNodeShutdown(healthSrv, grpcSrv.GracefulStop, defaultNodeShutdownGracePeriod)
	}()

	fmt.Fprintf(os.Stderr, "pillar-node: node-id=%s version=%s socket=%s\n",
		*nodeID, version, *csiSocket)
	serveErr := grpcSrv.Serve(lis)
	// Stop the trim loop (at most one chunk) before the iSCSI initiator it
	// may be trimming through is closed.
	stopTrim()
	// os.Exit skips defers: stop the metrics endpoint and flush spans
	// explicitly on both the clean and the error path.
	obs.shutdown()
	cancel()
	closeISCSIInitiator(iscsiInitiator)
	if serveErr != nil {
		fmt.Fprintf(os.Stderr, "pillar-node: serve: %v\n", serveErr)
		os.Exit(1)
	}
}

// listenCSISocketOrExit opens the CSI Unix socket at path, exiting the
// process non-zero on failure.
func listenCSISocketOrExit(path string) net.Listener {
	// Remove a stale socket file from a previous run so that net.Listen
	// does not fail with "address already in use".
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "pillar-node: remove stale socket %s: %v\n", path, err)
		os.Exit(1)
	}

	// Ensure the parent directory exists (kubelet creates it on modern
	// distributions, but guard here for dev/CI environments).
	socketDir := socketParentDir(path)
	if socketDir != "" {
		err = os.MkdirAll(socketDir, 0o750)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pillar-node: mkdir %s: %v\n", socketDir, err)
			os.Exit(1)
		}
	}

	lis, err := net.Listen("unix", path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pillar-node: listen unix %s: %v\n", path, err)
		os.Exit(1)
	}
	return lis
}

func runNodeShutdown(h *healthsrv.Server, gracefulStopFn func(), grace time.Duration) {
	h.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	time.Sleep(grace)
	gracefulStopFn()
}

// defaultTrimInterval is the --trim-interval default: weekly, like the
// fstrim.timer of util-linux.
const defaultTrimInterval = 7 * 24 * time.Hour

// trimIntervalUsage is the --trim-interval help text.
const trimIntervalUsage = "Interval between two periodic filesystem trims (FITRIM) of each staged " +
	"Filesystem-mode volume. 0 disables periodic trim."

// validateTrimIntervalOrExit exits the process non-zero when the
// --trim-interval value is negative.
func validateTrimIntervalOrExit(interval time.Duration) {
	if interval < 0 {
		fmt.Fprintf(os.Stderr, "pillar-node: --trim-interval %s must not be negative\n", interval)
		os.Exit(1)
	}
}

// startTrimmerOrExit starts the periodic trim of the node's staged
// Filesystem-mode volumes and returns the function that stops it.  An
// interval of 0 disables it.  A failure to start exits the process
// non-zero.
func startTrimmerOrExit(
	ctx context.Context, nodeSrv *csisvc.NodeServer, interval time.Duration, observer csisvc.TrimObserver,
) (stop func()) {
	if interval == 0 {
		fmt.Fprintln(os.Stderr, "pillar-node: periodic filesystem trim disabled (--trim-interval=0)")
		return func() {}
	}
	stop, err := nodeSrv.StartTrimmer(ctx, csisvc.TrimOptions{
		Interval:   interval,
		DriverName: driverName,
		Logger:     slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "trim"),
		Observer:   observer,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pillar-node: %v\n", err)
		os.Exit(1)
	}
	return stop
}

// nodeSysfsRoot is the production sysfs root.
const nodeSysfsRoot = "/sys"

// observabilityShutdownTimeout bounds each of the metrics server shutdown and
// the span flush.
const observabilityShutdownTimeout = 5 * time.Second

// nodeObservability holds what must be stopped before the process exits.
type nodeObservability struct {
	metricsSrv      *http.Server // nil when the endpoint is disabled
	shutdownTracing func(context.Context) error
	trim            *trimMetrics
}

// startObservability sets up tracing and starts the metrics endpoint,
// exiting the process on failure.
func startObservability(metricsAddr, version string) *nodeObservability {
	shutdownTracing, err := telemetry.Setup(context.Background(), telemetry.ComponentNode, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pillar-node: %v\n", err)
		os.Exit(1)
	}
	obs := &nodeObservability{shutdownTracing: shutdownTracing, trim: newTrimMetrics()}
	reg, err := newNodeMetricsRegistry(version, nodeSysfsRoot, obs.trim)
	if err == nil {
		obs.metricsSrv, err = startMetricsServer(metricsAddr, reg)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pillar-node: %v\n", err)
		obs.shutdown()
		os.Exit(1)
	}
	return obs
}

// shutdown stops the metrics endpoint and then flushes pending spans, each
// bounded by observabilityShutdownTimeout.  Failures are reported on stderr.
func (o *nodeObservability) shutdown() {
	if o.metricsSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), observabilityShutdownTimeout)
		err := o.metricsSrv.Shutdown(ctx)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pillar-node: metrics server shutdown: %v\n", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), observabilityShutdownTimeout)
	defer cancel()
	err := o.shutdownTracing(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pillar-node: %v\n", err)
	}
}

// newNodeGRPCServer returns the CSI gRPC server with the SP9 otelgrpc stats
// handler (Stage/Unstage/Publish/Unpublish/Expand traced) and the L1 failure
// log interceptor writing JSON lines to stderr.
func newNodeGRPCServer() *grpc.Server {
	failureLog := telemetry.SlogFailureLogger(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	return grpc.NewServer(
		grpc.StatsHandler(telemetry.NodeServerHandler()),
		grpc.ChainUnaryInterceptor(telemetry.UnaryServerFailureInterceptor(failureLog)),
	)
}

// restoreProtocolSessions re-applies the userspace-only session parameters
// (the iSCSI login timeout) to the sessions the initiator adopted from
// sysfs: kubelet does not repeat NodeStageVolume for volumes that stay
// mounted, e.g. across a pillar-node upgrade.  Not fatal: one volume whose
// session is gone must not keep the node from serving the others; each
// failure is logged.  The NVMe-oF request size cap of staged volumes is
// re-applied in the background (see
// csisvc.NodeServer.StartNVMeoFTransferLimitReconciler).
func restoreProtocolSessions(nodeSrv *csisvc.NodeServer) {
	logRestore := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "pillar-node: restore protocol sessions: "+format+"\n", args...)
	}
	restoreErr := nodeSrv.RestoreProtocolSessions(logRestore)
	if restoreErr != nil {
		logRestore("%v", restoreErr)
	}
}

// startTransferLimitReconciler keeps the NVMe-oF request size cap on the
// devices of staged volumes (see
// csisvc.NodeServer.StartNVMeoFTransferLimitReconciler).  It issues
// Identify Controller admin commands that can stall on an unresponsive
// target, so it runs in the background and never delays serving CSI calls.
// It runs until ctx is canceled; shutdown does not wait for it, since it
// only writes sysfs attributes and holds nothing another shutdown step
// closes.
func startTransferLimitReconciler(ctx context.Context, nodeSrv *csisvc.NodeServer) {
	_ = nodeSrv.StartNVMeoFTransferLimitReconciler(ctx, driverName,
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "nvmeof-transfer-limit"))
}

// startISCSIInitiatorOrExit starts the in-process iSCSI initiator when the
// kernel iscsi_tcp transport is available and returns it with this node's
// initiator IQN.  When iscsi_tcp is not loaded it logs why and returns
// (nil, ""): the iSCSI handler stays unregistered.  Any other failure exits
// the process non-zero, like a broken NVMe host identity does.
func startISCSIInitiatorOrExit(
	ctx context.Context, nameFile, netnsPath string,
) (initiator *iscsi.Initiator, iqn string) {
	availErr := iscsi.Available(nodeSysfsRoot)
	if availErr != nil {
		fmt.Fprintf(os.Stderr,
			"pillar-node: iSCSI initiator disabled: kernel module iscsi_tcp is not loaded (%v); "+
				"iscsi volumes cannot be staged on this node until iscsi_tcp is loaded and pillar-node restarts\n",
			availErr)
		return nil, ""
	}
	iqn, err := csisvc.ReadInitiatorIQN(nameFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pillar-node: read iSCSI initiator IQN: %v\n", err)
		os.Exit(1)
	}
	initiator, err = iscsi.NewInitiator(iscsi.Options{
		NetlinkNetnsPath:  netnsPath,
		OwnedTargetPrefix: iscsiOwnedTargetPrefix,
		// Only sessions logged in as this node's IQN are adopted: Kind
		// nodes share one kernel and see each other's sessions.
		InitiatorIQN: iqn,
		// Recovery and cleanup failures of the initiator must reach the
		// pod log; the zero logr.Logger would discard them.
		Logger: funcr.New(func(prefix, args string) {
			fmt.Fprintf(os.Stderr, "pillar-node: iscsi: %s %s\n", prefix, args)
		}, funcr.Options{}),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pillar-node: create iSCSI initiator (netlink netns %q): %v\n", netnsPath, err)
		os.Exit(1)
	}
	err = initiator.Start(ctx)
	if err != nil {
		closeISCSIInitiator(initiator)
		fmt.Fprintf(os.Stderr, "pillar-node: start iSCSI initiator (netlink netns %q): %v\n", netnsPath, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "pillar-node: iSCSI initiator enabled as %s\n", iqn)
	return initiator, iqn
}

// closeISCSIInitiator stops a started initiator, logging a failure.  It is
// a no-op for nil (iSCSI disabled).
func closeISCSIInitiator(initiator *iscsi.Initiator) {
	if initiator == nil {
		return
	}
	closeErr := initiator.Close()
	if closeErr != nil {
		fmt.Fprintf(os.Stderr, "pillar-node: close iSCSI initiator: %v\n", closeErr)
	}
}

// resolveHostIdentityOrExit reads (and on first start, generates) the local
// host NQN and host ID from /etc/nvme/{hostnqn,hostid}, exiting the process
// non-zero on failure.  Both values are required for every nvme-fabrics
// connect; see the fabricsConnector.hostNQN / hostID field docs for the
// kernel-vs-userland and EIO/EINVAL rationale.
func resolveHostIdentityOrExit() (hostNQN, hostID string) {
	var err error
	hostNQN, err = csisvc.ReadHostNQN()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pillar-node: read host NQN: %v\n", err)
		os.Exit(1)
	}
	hostID, err = csisvc.ReadHostID()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pillar-node: read host ID: %v\n", err)
		os.Exit(1)
	}
	return hostNQN, hostID
}

// publishNodeIdentity writes the NVMe host NQN — and iscsiIQN when the iSCSI
// initiator is enabled (non-empty) — to the CSINode object annotations using
// the in-cluster Kubernetes client.
//
// It retries up to 10 times with 3-second back-off to tolerate the race where
// kubelet has not yet created the CSINode object at driver registration time.
// Failures after all retries are logged but do not prevent the node plugin
// from starting — the controller will return FailedPrecondition for attach
// requests until the annotation is visible (RFC §5.2 degraded behavior).
func publishNodeIdentity(nodeName, iscsiIQN string) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"pillar-node: publishNodeIdentity: build in-cluster config: %v"+
				" (skipping CSINode annotation — running outside a cluster?)\n", err)
		return
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"pillar-node: publishNodeIdentity: create kube client: %v\n", err)
		return
	}

	patcher := csisvc.NewKubeCSINodePatcher(kubeClient)

	const maxRetries = 10
	const retryInterval = 3 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(maxRetries)*retryInterval+5*time.Second)
	defer cancel()

	for attempt := 1; attempt <= maxRetries; attempt++ {
		pubErr := csisvc.PublishNodeIdentity(ctx, patcher, nodeName, iscsiIQN)
		if pubErr == nil {
			fmt.Fprintf(os.Stderr,
				"pillar-node: published node identity annotations on CSINode %q\n", nodeName)
			return
		}
		fmt.Fprintf(os.Stderr,
			"pillar-node: publishNodeIdentity attempt %d/%d failed: %v\n",
			attempt, maxRetries, pubErr)
		if attempt < maxRetries {
			select {
			case <-ctx.Done():
				fmt.Fprintf(os.Stderr,
					"pillar-node: publishNodeIdentity: context canceled, giving up\n")
				return
			case <-time.After(retryInterval):
			}
		}
	}
	fmt.Fprintf(os.Stderr,
		"pillar-node: publishNodeIdentity: all %d attempts failed; "+
			"ControllerPublishVolume will return FailedPrecondition until annotation is set\n",
		maxRetries)
}

// socketParentDir returns the directory portion of the given socket path.
// Returns "" for a bare filename with no directory component.
func socketParentDir(socketPath string) string {
	idx := strings.LastIndex(socketPath, "/")
	if idx <= 0 {
		return ""
	}
	return socketPath[:idx]
}
