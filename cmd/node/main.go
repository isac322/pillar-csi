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
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"

	"google.golang.org/grpc"

	csi "github.com/container-storage-interface/spec/lib/go/csi"

	csisvc "github.com/bhyoo/pillar-csi/internal/csi"
)

// driverName is the CSI provisioner name declared in the StorageClass.
// It must match the name served by the controller plugin.
const driverName = "pillar-csi.bhyoo.com"

// ─────────────────────────────────────────────────────────────────────────────
// fabricsConnector — kernel-native NVMe-oF TCP connector
// ─────────────────────────────────────────────────────────────────────────────

// fabricsConnector implements the csisvc.Connector interface using the Linux
// /dev/nvme-fabrics kernel character device.  Unlike NVMeoFConnector it does
// NOT require nvme-cli to be installed in the container image — it speaks to
// the kernel NVMe-fabrics driver directly via the text-based write interface
// that has been available since Linux 4.15.
//
// Connect writes a comma-separated key=value option string to /dev/nvme-fabrics;
// the kernel nvme_fabrics module parses the string and initiates a TCP
// connection to the target.
//
// Disconnect removes each controller for the given subsystem NQN by writing
// "1" to its delete_controller sysfs entry.
//
// GetDevicePath scans /sys/class/nvme-subsystem/ for the matching NQN and
// returns the first namespace block device (nvmeXnY pattern) found there.
// When sysfs is unavailable (e.g. inside a Kubernetes pod with a restricted
// sysfs view), it falls back to scanning /dev/nvme*n* and querying each
// device's NQN via nvme-cli.
type fabricsConnector struct {
	// sysfsRoot is the root of the sysfs virtual filesystem.
	// Production value: "/sys".
	sysfsRoot string

	// fabricsDev is the path to the NVMe-fabrics character device.
	// Production value: "/dev/nvme-fabrics".
	fabricsDev string
}

// newFabricsConnector returns a production-ready fabricsConnector that uses
// /sys as the sysfs root and /dev/nvme-fabrics for connection requests.
func newFabricsConnector() *fabricsConnector {
	return &fabricsConnector{
		sysfsRoot:  "/sys",
		fabricsDev: "/dev/nvme-fabrics",
	}
}

// Connect establishes an NVMe-oF TCP connection to the given subsystem NQN
// at the given transport address (trAddr) and service ID (TCP port, trSvcID).
//
// It is idempotent: if the subsystem NQN is already connected (detected by
// scanning /sys/class/nvme-subsystem/ or via nvme-cli) the method returns nil
// immediately.
//
// On a new connection it opens /dev/nvme-fabrics and writes:
//
//	transport=tcp,traddr=<trAddr>,trsvcid=<trSvcID>,nqn=<subsysNQN>
//
// The kernel nvme_fabrics module parses the string, creates the controller,
// and initiates the TCP connection synchronously.  Write returns an error if
// the connection fails (target unreachable, invalid NQN, etc.).
func (c *fabricsConnector) Connect(ctx context.Context, subsysNQN, trAddr, trSvcID string) error {
	already, err := c.isConnected(ctx, subsysNQN)
	if err != nil {
		return fmt.Errorf("fabricsConnector Connect: check existing connection for %q: %w", subsysNQN, err)
	}
	if already {
		return nil
	}

	f, err := os.OpenFile(c.fabricsDev, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("fabricsConnector Connect: open %s: %w", c.fabricsDev, err)
	}
	defer func() { _ = f.Close() }() //nolint:errcheck

	// Write the connection parameters as a comma-separated key=value string.
	// The kernel nvme_fabrics driver parses this in nvmf_dev_write() and
	// initiates the TCP connection via nvmf_create_ctrl().
	opts := fmt.Sprintf("transport=tcp,traddr=%s,trsvcid=%s,nqn=%s", trAddr, trSvcID, subsysNQN)
	_, err = fmt.Fprintf(f, "%s\n", opts)
	if err != nil {
		return fmt.Errorf("fabricsConnector Connect: write to %s (nqn=%s): %w",
			c.fabricsDev, subsysNQN, err)
	}
	return nil
}

// Disconnect tears down all NVMe-oF controllers associated with the given
// subsystem NQN by writing "1" to each controller's delete_controller sysfs
// entry.
//
// It is idempotent: if the NQN is not connected the method returns nil
// immediately.
func (c *fabricsConnector) Disconnect(_ context.Context, subsysNQN string) error {
	subsysDir := filepath.Join(c.sysfsRoot, "class", "nvme-subsystem")

	entries, err := os.ReadDir(subsysDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("fabricsConnector Disconnect: read %s: %w", subsysDir, err)
	}

	for _, entry := range entries {
		nqnFile := filepath.Join(subsysDir, entry.Name(), "subsysnqn")
		nqnBytes, readErr := os.ReadFile(nqnFile) //nolint:gosec
		if readErr != nil || strings.TrimSpace(string(nqnBytes)) != subsysNQN {
			continue
		}
		// Found the matching subsystem.  Scan for controller entries (nvmeX,
		// not namespace entries nvmeXnY) and delete each one.
		subsysPath := filepath.Join(subsysDir, entry.Name())
		ctrlEntries, readErr := os.ReadDir(subsysPath)
		if readErr != nil {
			continue
		}
		for _, ctrlEntry := range ctrlEntries {
			name := ctrlEntry.Name()
			if !strings.HasPrefix(name, "nvme") {
				continue
			}
			// Namespace entries match nvmeXnY (contain 'n' after the controller
			// number digits); controller entries are nvmeX (no 'n' in suffix).
			suffix := strings.TrimPrefix(name, "nvme")
			if strings.ContainsRune(suffix, 'n') {
				continue // skip namespace entries
			}
			deletePath := filepath.Join(c.sysfsRoot, "class", "nvme", name, "delete_controller")
			_ = os.WriteFile(deletePath, []byte("1"), 0o600) //nolint:errcheck
		}
	}
	return nil
}

// GetDevicePath returns the /dev/nvmeXnY block-device path for the given
// subsystem NQN after a successful Connect call.
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
func (c *fabricsConnector) GetDevicePath(ctx context.Context, subsysNQN string) (string, error) { //nolint:gocognit
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
				// Namespace entry nvmeXnY found in sysfs.
				devPath := "/dev/" + name
				_, statErr := os.Stat(devPath)
				if statErr == nil {
					return devPath, nil
				}
				// Namespace visible in sysfs but device node not yet in /dev/.
				// The test bridge goroutine (or udev) will create the node
				// shortly; return "" to keep polling.
				return "", nil
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
			fmt.Fprintf(os.Stderr,
				"pillar-node: GetDevicePath: NQN %q found in sysfs but no "+
					"namespace in subsystem dir %s; falling back to nvme-cli\n",
				subsysNQN, subsysPath)
			break
		}
		if !nqnFound {
			// The subsystem NQN was not found in sysfs at all.  This means
			// Connect() has not yet completed (or the subsystem is not yet
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
func (c *fabricsConnector) getDevicePathViaController(subsysPath string) (string, error) { //nolint:gocognit,gocyclo
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
			devPath := "/dev/" + nsName
			// Device node already exists → use it.
			_, statErr := os.Stat(devPath)
			if statErr == nil {
				fmt.Fprintf(os.Stderr,
					"pillar-node: getDevicePathViaController: found existing %s\n", devPath)
				return devPath, nil
			}
			// Device node missing.  Read major:minor from sysfs "dev" file and
			// create the block device node via mknod(2).
			devFile := filepath.Join(ctrlSysPath, nsName, "dev")
			devBytes, readErr := os.ReadFile(devFile) //nolint:gosec
			if readErr != nil {
				fmt.Fprintf(os.Stderr,
					"pillar-node: getDevicePathViaController: read %s: %v\n", devFile, readErr)
				continue
			}
			parts := strings.SplitN(strings.TrimSpace(string(devBytes)), ":", 2)
			if len(parts) != 2 {
				continue
			}
			major, majErr := strconv.ParseUint(parts[0], 10, 32)
			minor, minErr := strconv.ParseUint(parts[1], 10, 32)
			if majErr != nil || minErr != nil {
				continue
			}
			// Compute device number using Linux makedev formula.
			// minor bits 0-7 → bits 0-7; major bits 0-11 → bits 8-19;
			// minor bits 8-19 → bits 20-31; major bits 12+ → bits 32+.
			dev := int((minor & 0xff) | ((major & 0xfff) << 8) | //nolint:gosec // G115: Linux makedev bit-packing
				((minor &^ 0xff) << 12) | ((major &^ 0xfff) << 32))
			mknodErr := syscall.Mknod(devPath, syscall.S_IFBLK|0o600, dev)
			if mknodErr != nil && !os.IsExist(mknodErr) {
				fmt.Fprintf(os.Stderr,
					"pillar-node: getDevicePathViaController: mknod %s (%d:%d): %v\n",
					devPath, major, minor, mknodErr)
				continue
			}
			fmt.Fprintf(os.Stderr,
				"pillar-node: getDevicePathViaController: created %s (%d:%d)\n",
				devPath, major, minor)
			return devPath, nil
		}
	}
	return "", nil
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
func (c *fabricsConnector) isConnected(ctx context.Context, subsysNQN string) (bool, error) { //nolint:unparam
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
			if strings.TrimSpace(string(nqnBytes)) == subsysNQN {
				return true, nil
			}
		}
		// Sysfs is readable; subsystem not found → not connected.
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
func (m *mkdirMounter) FormatAndMount(source, target, fsType string, options []string) error {
	mkdirErr := os.MkdirAll(target, 0o750)
	if mkdirErr != nil {
		return fmt.Errorf("mkdirMounter: create mount target %q: %w", target, mkdirErr)
	}
	return m.wrapped.FormatAndMount(source, target, fsType, options)
}

// Mount creates the target directory if it does not exist (required for
// NodePublishVolume bind mounts when the CO has not yet pre-created the
// target pod volume path), then delegates to the wrapped Mounter's Mount.
func (m *mkdirMounter) Mount(source, target, fsType string, options []string) error {
	mkdirErr := os.MkdirAll(target, 0o750)
	if mkdirErr != nil {
		return fmt.Errorf("mkdirMounter: create mount target %q: %w", target, mkdirErr)
	}
	return m.wrapped.Mount(source, target, fsType, options)
}

// Unmount delegates to the wrapped Mounter unchanged.
func (m *mkdirMounter) Unmount(target string) error {
	return m.wrapped.Unmount(target)
}

// IsMounted delegates to the wrapped Mounter unchanged.
func (m *mkdirMounter) IsMounted(target string) (bool, error) {
	return m.wrapped.IsMounted(target)
}

// ─────────────────────────────────────────────────────────────────────────────
// main
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	nodeID := flag.String("node-id", "",
		"Unique identifier for this Kubernetes node (typically the Node name). Required.")
	csiSocket := flag.String("csi-socket", "/var/lib/kubelet/plugins/pillar-csi.bhyoo.com/csi.sock",
		"Path to the Unix domain socket on which the CSI gRPC server listens.")
	flag.Parse()

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
	version := "dev"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}

	// ── Build the CSI service implementations ──────────────────────────────
	// fabricsConnector uses the Linux /dev/nvme-fabrics kernel interface for
	// NVMe-oF TCP connections.  It does not require nvme-cli to be installed
	// in the container image, which simplifies the Dockerfile.
	identitySrv := csisvc.NewIdentityServer(driverName, version)
	nodeSrv := csisvc.NewNodeServer(*nodeID, newFabricsConnector(), &mkdirMounter{wrapped: csisvc.NewKubeMounter()})

	// ── Open the Unix socket ───────────────────────────────────────────────
	// Remove a stale socket file from a previous run so that net.Listen
	// does not fail with "address already in use".
	err := os.Remove(*csiSocket)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "pillar-node: remove stale socket %s: %v\n", *csiSocket, err)
		os.Exit(1)
	}

	// Ensure the parent directory exists (kubelet creates it on modern
	// distributions, but guard here for dev/CI environments).
	socketDir := socketParentDir(*csiSocket)
	if socketDir != "" {
		err = os.MkdirAll(socketDir, 0o750)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pillar-node: mkdir %s: %v\n", socketDir, err)
			os.Exit(1)
		}
	}

	lis, err := net.Listen("unix", *csiSocket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pillar-node: listen unix %s: %v\n", *csiSocket, err)
		os.Exit(1)
	}

	// ── Register and start the gRPC server ────────────────────────────────
	grpcSrv := grpc.NewServer()
	csi.RegisterIdentityServer(grpcSrv, identitySrv)
	csi.RegisterNodeServer(grpcSrv, nodeSrv)

	// Graceful shutdown on SIGTERM / SIGINT.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigs
		fmt.Fprintf(os.Stderr, "pillar-node: received %s, shutting down\n", sig)
		grpcSrv.GracefulStop()
	}()

	fmt.Fprintf(os.Stderr, "pillar-node: node-id=%s version=%s socket=%s\n",
		*nodeID, version, *csiSocket)
	serveErr := grpcSrv.Serve(lis)
	if serveErr != nil {
		fmt.Fprintf(os.Stderr, "pillar-node: serve: %v\n", serveErr)
		os.Exit(1)
	}
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
