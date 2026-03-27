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
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
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
// main
// ─────────────────────────────────────────────────────────────────────────────.

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
	identitySrv := csisvc.NewIdentityServer(driverName, version)
	nodeSrv := csisvc.NewNodeServer(*nodeID, csisvc.NewNVMeoFConnector(), csisvc.NewKubeMounter())

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
