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
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	healthsrv "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	pillarv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/agent/backend/lvm"
	"github.com/bhyoo/pillar-csi/internal/agent/backend/zfs"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
	"github.com/bhyoo/pillar-csi/internal/runtimepaths"
	"github.com/bhyoo/pillar-csi/internal/tlscreds"
)

// buildVolumeBackends constructs the pool→backend registry from the agent
// config file's backends entries.  For ZFS backends the registry key is the
// pool name.  For LVM backends the registry key is the VG name (used as the
// "pool" prefix in VolumeIDs of the form "<vg>/<lv-name>").
//
// The key must be unique across all entries: agent RPCs route a volume to its
// backend by the volume ID's first path component alone (pool name or VG
// name), so two backends sharing a key are indistinguishable.  Rather than
// silently dropping all but the last entry, a duplicate key is a fatal
// configuration error.
//
// Per-volume settings (LVM provisioningMode) are resolved by the controller
// and sent with each CreateVolume, so the backend only needs placement.
func buildVolumeBackends(specs []pillarv1alpha1.BackendSpec) (map[string]backend.VolumeBackend, error) {
	m := make(map[string]backend.VolumeBackend, len(specs))
	seen := make(map[string]int, len(specs))
	for i, spec := range specs {
		key := spec.PoolName()
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf(
				"agent config: duplicate pool/VG %q: backends[%d] (%s) conflicts with backends[%d] (%s); "+
					"pool/VG names must be unique across backends",
				key, i, spec.Kind(), prev, specs[prev].Kind())
		}
		seen[key] = i
		switch {
		case spec.ZFS != nil:
			m[key] = zfs.New(spec.ZFS.Pool, spec.ZFS.ParentDataset)
		case spec.LVM != nil:
			mode := lvm.ProvisionModeLinear
			if spec.LVM.ProvisioningMode == pillarv1alpha1.LVMProvisioningModeThin {
				mode = lvm.ProvisionModeThin
			}
			m[key] = lvm.New(spec.LVM.VolumeGroup, spec.LVM.ThinPool).WithMode(mode)
		default:
			return nil, fmt.Errorf("agent config: backends[%d]: exactly one of lvm, zfs must be set", i)
		}
	}
	return m, nil
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
	listenAddr := flag.String("listen-address", ":9500", "gRPC listen address (host:port)")
	gracePeriod := flag.Duration("shutdown-grace-period", 5*time.Second,
		"Time to wait between health=NOT_SERVING and GracefulStop, giving "+
			"any already-routed RPCs time to complete.")
	configPath := flag.String("config", "",
		"Path to the backend placement config file (required). YAML, same shape as the chart's agent.backends:\n"+
			"  backends:\n"+
			"    - zfs: {pool: tank, parentDataset: k8s}\n"+
			"    - lvm: {volumeGroup: data-vg, thinPool: thin-pool-0}")
	cfgRoot := flag.String("configfs-root", resolvedDefaultConfigfsRoot(),
		"nvmet configfs root directory (override in tests)")
	tlsCert := flag.String("tls-cert", "", "path to PEM server certificate for mTLS")
	tlsKey := flag.String("tls-key", "", "path to PEM server private key for mTLS")
	tlsCA := flag.String("tls-ca", "", "path to PEM CA certificate for mTLS client verification")

	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "error: --config is required (path to the agent backend config file)")
		os.Exit(1)
	}

	tlsEnabled := *tlsCert != "" || *tlsKey != "" || *tlsCA != ""
	if tlsEnabled && (*tlsCert == "" || *tlsKey == "" || *tlsCA == "") {
		fmt.Fprintln(os.Stderr, "error: --tls-cert, --tls-key, and --tls-ca must all be provided together")
		os.Exit(1)
	}

	backendSpecs, err := loadAgentConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	volumeBackends, err := buildVolumeBackends(backendSpecs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	// The agent starts with the export restore pending: it re-creates no
	// export until the controller sent the complete export state in one
	// ReconcileState, so a shared NVMe/TCP port starts listening only after
	// every export on it is ready (issue #92).
	srv := agent.NewServer(volumeBackends, *cfgRoot, agent.WithExportRestoreGate())

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

	grpcSrv, healthSrv := newAgentGRPCServer(srv, grpcOpts)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		runAgentShutdown(healthSrv, func(ctx context.Context) error {
			_, drainErr := srv.Drain(ctx, &agentv1.DrainRequest{})
			return drainErr
		}, grpcSrv.GracefulStop, *gracePeriod)
	}()

	fmt.Fprintf(os.Stderr, "pillar-agent listening on %s\n", *listenAddr)
	serveErr := grpcSrv.Serve(lis)
	if serveErr != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", serveErr)
		os.Exit(1)
	}
}

func runAgentShutdown(
	h *healthsrv.Server,
	drainFn func(context.Context) error,
	gracefulStopFn func(),
	grace time.Duration,
) {
	h.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drainErr := drainFn(ctx)
	if drainErr != nil {
		fmt.Fprintf(os.Stderr, "pillar-agent: drain failed: %v\n", drainErr)
	}
	time.Sleep(grace)
	gracefulStopFn()
}

func resolvedDefaultConfigfsRoot() string {
	return runtimepaths.ResolveAgentConfigfsRoot(nvmeof.DefaultConfigfsRoot)
}
