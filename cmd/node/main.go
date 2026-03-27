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
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
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
// scanning /sys/class/nvme-subsystem/) the method returns nil immediately.
//
// On a new connection it opens /dev/nvme-fabrics and writes:
//
//	transport=tcp,traddr=<trAddr>,trsvcid=<trSvcID>,nqn=<subsysNQN>
//
// The kernel nvme_fabrics module parses the string, creates the controller,
// and initiates the TCP connection synchronously.  Write returns an error if
// the connection fails (target unreachable, invalid NQN, etc.).
func (c *fabricsConnector) Connect(_ context.Context, subsysNQN, trAddr, trSvcID string) error {
	already, err := c.isConnected(subsysNQN)
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
// It scans /sys/class/nvme-subsystem/ looking for a subsystem whose
// "subsysnqn" file matches subsysNQN, then finds the first namespace entry
// (nvmeXnY pattern) within that subsystem directory and constructs the /dev/
// path.
//
// Returns ("", nil) when the device is not yet visible in sysfs; callers
// should poll until a non-empty path is returned or a deadline is exceeded.
func (c *fabricsConnector) GetDevicePath(_ context.Context, subsysNQN string) (string, error) {
	subsysDir := filepath.Join(c.sysfsRoot, "class", "nvme-subsystem")

	entries, err := os.ReadDir(subsysDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("fabricsConnector GetDevicePath: read %s: %w", subsysDir, err)
	}

	for _, entry := range entries {
		subsysPath := filepath.Join(subsysDir, entry.Name())
		nqnFile := filepath.Join(subsysPath, "subsysnqn")
		nqnBytes, readErr := os.ReadFile(nqnFile) //nolint:gosec
		if readErr != nil || strings.TrimSpace(string(nqnBytes)) != subsysNQN {
			continue
		}
		nsEntries, readErr := os.ReadDir(subsysPath)
		if readErr != nil {
			return "", fmt.Errorf("fabricsConnector GetDevicePath: read %s: %w", subsysPath, readErr)
		}
		for _, nsEntry := range nsEntries {
			name := nsEntry.Name()
			if strings.HasPrefix(name, "nvme") && strings.Contains(name, "n") {
				suffix := strings.TrimPrefix(name, "nvme")
				if strings.ContainsRune(suffix, 'n') {
					return "/dev/" + name, nil
				}
			}
		}
		// Subsystem found but no namespace device visible yet.
		return "", nil
	}
	return "", nil
}

// isConnected returns true when the given NQN has an entry in
// /sys/class/nvme-subsystem/, indicating an active NVMe-oF connection.
func (c *fabricsConnector) isConnected(subsysNQN string) (bool, error) {
	subsysDir := filepath.Join(c.sysfsRoot, "class", "nvme-subsystem")

	entries, err := os.ReadDir(subsysDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", subsysDir, err)
	}
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
	return false, nil
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
	nodeSrv := csisvc.NewNodeServer(*nodeID, newFabricsConnector(), csisvc.NewKubeMounter())

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
