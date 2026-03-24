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

// Package main is the entry point for the pillar-csi storage-node agent.
// It exposes the AgentService gRPC API used by the pillar-controller to
// manage ZFS zvol volumes and NVMe-oF TCP exports on this node.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"google.golang.org/grpc"

	"github.com/bhyoo/pillar-csi/internal/agent"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/agent/backend/zfs"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// poolsFlag is a repeatable string flag that accumulates one ZFS pool name
// per --zfs-pool invocation.  It satisfies the flag.Value interface so that
// the standard flag package can be used without importing a third-party CLI
// library.
//
// Usage:
//
//	pillar-agent --zfs-pool tank --zfs-pool hot-data
type poolsFlag []string

// String returns a comma-separated representation of all collected pool names.
// The flag package calls this when printing usage/defaults.
func (p *poolsFlag) String() string {
	if p == nil || len(*p) == 0 {
		return ""
	}
	return strings.Join(*p, ",")
}

// Set appends a single pool name to the slice.  Called by flag.Parse for
// each --zfs-pool occurrence on the command line.
func (p *poolsFlag) Set(v string) error {
	if v == "" {
		return fmt.Errorf("pool name must not be empty")
	}
	*p = append(*p, v)
	return nil
}

func main() {
	listenAddr := flag.String("listen-address", ":50051", "gRPC listen address (host:port)")
	var zfsPools poolsFlag
	flag.Var(&zfsPools, "zfs-pool",
		"ZFS pool name managed by this agent; may be repeated for multiple pools (required)")
	zfsParent := flag.String("zfs-parent-dataset", "", "ZFS parent dataset within each pool (optional)")
	cfgRoot := flag.String("configfs-root", nvmeof.DefaultConfigfsRoot,
		"nvmet configfs root directory (override in tests)")
	flag.Parse()

	if len(zfsPools) == 0 {
		fmt.Fprintln(os.Stderr, "error: --zfs-pool is required (flag may be repeated for multiple pools)")
		os.Exit(1)
	}

	backends := make(map[string]backend.VolumeBackend, len(zfsPools))
	for _, pool := range zfsPools {
		backends[pool] = zfs.New(pool, *zfsParent)
	}

	srv := agent.NewServer(backends, *cfgRoot)

	lis, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", *listenAddr, err)
		os.Exit(1)
	}

	grpcSrv := grpc.NewServer()
	srv.Register(grpcSrv)

	// Graceful shutdown: stop accepting new RPCs on SIGTERM / SIGINT, then
	// wait for in-flight handlers to complete before exiting.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		grpcSrv.GracefulStop()
	}()

	fmt.Fprintf(os.Stderr, "pillar-agent listening on %s\n", *listenAddr)
	serveErr := grpcSrv.Serve(lis)
	if serveErr != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", serveErr)
		os.Exit(1)
	}
}
