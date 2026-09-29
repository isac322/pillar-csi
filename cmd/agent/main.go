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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	healthsrv "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	pillarv1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/backend/lvm"
	"github.com/isac322/pillar-csi/internal/agent/backend/zfs"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
	"github.com/isac322/pillar-csi/internal/runtimepaths"
	"github.com/isac322/pillar-csi/internal/telemetry"
	"github.com/isac322/pillar-csi/internal/tlscreds"
)

// observabilityShutdownTimeout bounds each of the metrics server shutdown
// and the span flush on exit.
const observabilityShutdownTimeout = 5 * time.Second

// metricsReadHeaderTimeout bounds how long the metrics server waits for a
// scrape's request headers.
const metricsReadHeaderTimeout = 10 * time.Second

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
	metricsAddr := flag.String("metrics-bind-address", "0",
		"Address (host:port) the Prometheus /metrics endpoint binds to. \"0\" disables it.")
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

	serveAgent(srv, serveConfig{
		listenAddr:  *listenAddr,
		metricsAddr: *metricsAddr,
		gracePeriod: *gracePeriod,
		tlsEnabled:  tlsEnabled,
		tlsCert:     *tlsCert,
		tlsKey:      *tlsKey,
		tlsCA:       *tlsCA,
	})
}

// serveConfig is the serving part of the agent's command line.
type serveConfig struct {
	listenAddr  string
	metricsAddr string
	gracePeriod time.Duration
	tlsEnabled  bool
	tlsCert     string
	tlsKey      string
	tlsCA       string
}

// serveAgent sets up telemetry and the metrics endpoint, then serves srv
// over gRPC until the server stops.  It exits the process on failure.
func serveAgent(srv *agent.Server, cfg serveConfig) {
	version, _ := telemetry.BuildVersion()
	shutdownTelemetry, err := telemetry.Setup(context.Background(), telemetry.ComponentAgent, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: telemetry setup: %v\n", err)
		os.Exit(1)
	}
	// os.Exit skips defers, so every exit path below calls fail, which
	// shuts the metrics server down and flushes pending spans first.
	var metricsSrv *http.Server
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format, args...)
		shutdownObservability(metricsSrv, shutdownTelemetry)
		os.Exit(1)
	}

	serverMetrics := newAgentServerMetrics()
	reg, err := newAgentRegistry(srv, serverMetrics, version)
	if err != nil {
		fail("error: %v\n", err)
	}
	metricsSrv, err = startMetricsServer(cfg.metricsAddr, reg)
	if err != nil {
		fail("error: %v\n", err)
	}

	lis, err := net.Listen("tcp", cfg.listenAddr)
	if err != nil {
		fail("listen %s: %v\n", cfg.listenAddr, err)
	}

	grpcOpts, err := buildGRPCOpts(cfg.tlsEnabled, cfg.tlsCert, cfg.tlsKey, cfg.tlsCA)
	if err != nil {
		fail("error: %v\n", err)
	}

	failureLog := telemetry.SlogFailureLogger(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	grpcSrv, healthSrv := newAgentGRPCServer(srv, serverMetrics, failureLog, grpcOpts)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		runAgentShutdown(healthSrv, func(ctx context.Context) error {
			_, drainErr := srv.Drain(ctx, &agentv1.DrainRequest{})
			return drainErr
		}, grpcSrv.GracefulStop, cfg.gracePeriod)
	}()

	fmt.Fprintf(os.Stderr, "pillar-agent listening on %s\n", cfg.listenAddr)
	serveErr := grpcSrv.Serve(lis)
	if serveErr != nil {
		fail("serve: %v\n", serveErr)
	}
	shutdownObservability(metricsSrv, shutdownTelemetry)
}

// newAgentRegistry builds the agent's Prometheus registry: Go and process
// collectors, pillar_csi_build_info, exec and certificate metrics, the
// agent's own metrics, and the gRPC server metrics.
func newAgentRegistry(
	srv *agent.Server,
	serverMetrics *grpcprom.ServerMetrics,
	version string,
) (*prometheus.Registry, error) {
	reg := prometheus.NewRegistry()
	for _, c := range []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		telemetry.BuildInfoCollector(telemetry.ComponentAgent, version),
		serverMetrics,
	} {
		err := reg.Register(c)
		if err != nil {
			return nil, fmt.Errorf("register agent metrics collector: %w", err)
		}
	}
	err := telemetry.RegisterExecMetrics(reg)
	if err != nil {
		return nil, fmt.Errorf("register exec metrics: %w", err)
	}
	err = telemetry.RegisterCertificateMetrics(reg)
	if err != nil {
		return nil, fmt.Errorf("register certificate metrics: %w", err)
	}
	err = srv.RegisterMetrics(reg)
	if err != nil {
		return nil, fmt.Errorf("register agent metrics: %w", err)
	}
	return reg, nil
}

// startMetricsServer serves reg on /metrics (OpenMetrics negotiated, so
// exemplars are exposed) at addr.  Addr "0" disables the endpoint and
// returns a nil server.  A listen failure is returned; a later serve failure
// is reported on stderr.
func startMetricsServer(addr string, reg *prometheus.Registry) (*http.Server, error) {
	if addr == "0" {
		return nil, nil //nolint:nilnil // a nil server means the endpoint is disabled.
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen metrics %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true}))
	metricsSrv := &http.Server{Handler: mux, ReadHeaderTimeout: metricsReadHeaderTimeout}
	go func() {
		serveErr := metricsSrv.Serve(lis)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "pillar-agent: metrics server on %s stopped: %v\n", addr, serveErr)
		}
	}()
	fmt.Fprintf(os.Stderr, "pillar-agent: serving metrics on %s\n", addr)
	return metricsSrv, nil
}

// shutdownObservability stops the metrics server and then flushes pending
// spans, each within observabilityShutdownTimeout.  Failures are reported on
// stderr: the process is exiting either way.
func shutdownObservability(metricsSrv *http.Server, shutdownTelemetry func(context.Context) error) {
	if metricsSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), observabilityShutdownTimeout)
		err := metricsSrv.Shutdown(ctx)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pillar-agent: metrics server shutdown: %v\n", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), observabilityShutdownTimeout)
	defer cancel()
	err := shutdownTelemetry(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pillar-agent: telemetry shutdown: %v\n", err)
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
