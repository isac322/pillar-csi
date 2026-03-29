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
	"github.com/bhyoo/pillar-csi/internal/tlscreds"
)

// backendSpec holds the parsed fields from a single --backend flag value.
// The flag value is a comma-separated list of key=value pairs, e.g.:
//
//	type=zfs-zvol,pool=tank
//	type=zfs-zvol,pool=hot-data,parent=k8s
type backendSpec struct {
	// typ is the backend type identifier.  Currently only "zfs-zvol" is
	// supported.
	typ string

	// pool is the storage pool name (ZFS pool for type=zfs-zvol).
	pool string

	// parent is the optional parent dataset / volume-group path within the
	// pool.  For ZFS this maps to the parentDataset argument of zfs.New.
	parent string
}

// backendFlag is a repeatable flag that accumulates one backendSpec per
// --backend invocation.  It satisfies flag.Value so that the standard flag
// package can be used without a third-party CLI library.
//
// Usage:
//
//	pillar-agent --backend type=zfs-zvol,pool=tank
//	pillar-agent --backend type=zfs-zvol,pool=tank,parent=k8s --backend type=zfs-zvol,pool=hot-data
type backendFlag []backendSpec

// String returns a human-readable summary of all registered backend specs.
// The flag package calls this when printing usage / defaults.
func (b *backendFlag) String() string {
	if b == nil || len(*b) == 0 {
		return ""
	}
	parts := make([]string, len(*b))
	for i, s := range *b {
		parts[i] = "type=" + s.typ + ",pool=" + s.pool
		if s.parent != "" {
			parts[i] += ",parent=" + s.parent
		}
	}
	return strings.Join(parts, " ")
}

// Set parses a single --backend flag value and appends the resulting
// backendSpec.  Called by flag.Parse for each --backend occurrence.
//
// The value must contain at least type=<t> and pool=<p> keys.  An unknown
// key causes an error so that typos are caught early.  Supported keys:
//
//	type   – backend type; currently only "zfs-zvol" is accepted
//	pool   – pool / volume-group name (required)
//	parent – parent dataset within the pool (optional)
func (b *backendFlag) Set(v string) error {
	if v == "" {
		return fmt.Errorf("backend: value must not be empty")
	}

	spec := backendSpec{}
	for kv := range strings.SplitSeq(v, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("backend: %q is not a key=value pair", kv)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "type":
			spec.typ = val
		case "pool":
			spec.pool = val
		case "parent":
			spec.parent = val
		default:
			return fmt.Errorf("backend: unknown key %q (supported: type, pool, parent)", key)
		}
	}

	if spec.typ == "" {
		return fmt.Errorf("backend: type= key is required")
	}
	if spec.pool == "" {
		return fmt.Errorf("backend: pool= key is required")
	}
	if spec.typ != "zfs-zvol" {
		return fmt.Errorf("backend: unsupported type %q (supported: zfs-zvol)", spec.typ)
	}

	*b = append(*b, spec)
	return nil
}

// buildVolumeBackends constructs the pool→backend registry from --backend flags.
func buildVolumeBackends(specs backendFlag) map[string]backend.VolumeBackend {
	m := make(map[string]backend.VolumeBackend, len(specs))
	for _, spec := range specs {
		m[spec.pool] = zfs.New(spec.pool, spec.parent)
	}
	return m
}

// buildGRPCOpts returns the gRPC server options for the given TLS
// configuration.  When tlsEnabled is true all three PEM paths must be valid;
// an error is returned if the credentials cannot be loaded.
func buildGRPCOpts(tlsEnabled bool, cert, key, ca string) ([]grpc.ServerOption, error) {
	if !tlsEnabled {
		fmt.Fprintln(os.Stderr, "pillar-agent: WARNING: starting in plaintext mode (no --tls-cert/--tls-key/--tls-ca flags)")
		return nil, nil
	}
	creds, err := tlscreds.LoadServerCredentials(cert, key, ca)
	if err != nil {
		return nil, fmt.Errorf("load TLS credentials: %w", err)
	}
	fmt.Fprintf(os.Stderr, "pillar-agent: mTLS enabled (cert=%s, ca=%s)\n", cert, ca)
	return []grpc.ServerOption{grpc.Creds(creds)}, nil
}

func main() {
	listenAddr := flag.String("listen-address", ":50051", "gRPC listen address (host:port)")

	// --backend: pluggable backend flag.  Format: type=<t>,pool=<p>[,parent=<ds>]
	var backends backendFlag
	flag.Var(&backends, "backend",
		"Backend spec as comma-separated key=value pairs.\n"+
			"Required: type (zfs-zvol), pool.  Optional: parent.\n"+
			"Example: --backend type=zfs-zvol,pool=tank,parent=k8s")

	cfgRoot := flag.String("configfs-root", nvmeof.DefaultConfigfsRoot,
		"nvmet configfs root directory (override in tests)")
	tlsCert := flag.String("tls-cert", "", "path to PEM server certificate for mTLS")
	tlsKey := flag.String("tls-key", "", "path to PEM server private key for mTLS")
	tlsCA := flag.String("tls-ca", "", "path to PEM CA certificate for mTLS client verification")

	flag.Parse()

	if len(backends) == 0 {
		fmt.Fprintln(os.Stderr,
			"error: at least one backend is required: use --backend type=zfs-zvol,pool=<name>")
		os.Exit(1)
	}

	tlsEnabled := *tlsCert != "" || *tlsKey != "" || *tlsCA != ""
	if tlsEnabled && (*tlsCert == "" || *tlsKey == "" || *tlsCA == "") {
		fmt.Fprintln(os.Stderr, "error: --tls-cert, --tls-key, and --tls-ca must all be provided together")
		os.Exit(1)
	}

	volumeBackends := buildVolumeBackends(backends)
	srv := agent.NewServer(volumeBackends, *cfgRoot)

	lis, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", *listenAddr, err)
		os.Exit(1)
	}

	grpcOpts, err := buildGRPCOpts(tlsEnabled, *tlsCert, *tlsKey, *tlsCA)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	grpcSrv := grpc.NewServer(grpcOpts...)
	srv.Register(grpcSrv)

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
