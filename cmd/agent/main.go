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
	"github.com/isac322/pillar-csi/internal/agent/nfs"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
	"github.com/isac322/pillar-csi/internal/runtimepaths"
	"github.com/isac322/pillar-csi/internal/telemetry"
	"github.com/isac322/pillar-csi/internal/tlscreds"
)

const (
	// ObservabilityShutdownTimeout bounds each of the metrics server shutdown
	// and the span flush on exit.
	observabilityShutdownTimeout = 5 * time.Second
	// MetricsReadHeaderTimeout bounds how long the metrics server waits for a
	// scrape's request headers.
	metricsReadHeaderTimeout = 10 * time.Second
	agentDataRoot            = "/var/lib/pillar-csi/agent"
	agentDatasetRoot         = agentDataRoot + "/datasets"
	agentNFSStateRoot        = agentDataRoot + "/nfs"
)

// buildVolumeBackends constructs the pool→backend registry from the agent
// config file's backends entries. A ZFS pool may have one zvol and one
// dataset backend; exact (pool, backend type) duplicates are rejected.
func buildVolumeBackends(
	specs []pillarv1alpha1.BackendSpec,
	configfsRoot string,
) (map[string]backend.VolumeBackend, error) {
	registries, _, err := buildVolumeBackendRegistry(specs, configfsRoot, agentDatasetRoot)
	return registries, err
}

func buildVolumeBackendRegistry(
	specs []pillarv1alpha1.BackendSpec,
	configfsRoot, datasetRoot string,
) (
	registries map[string]backend.VolumeBackend,
	variantRegistry map[string]map[agentv1.BackendType]backend.VolumeBackend,
	err error,
) {
	registries = make(map[string]backend.VolumeBackend, len(specs))
	variantRegistry = make(map[string]map[agentv1.BackendType]backend.VolumeBackend, len(specs))
	seen := make(map[string]int, len(specs))
	for i, spec := range specs {
		key := spec.PoolName()
		typ, err := backendTypeForSpec(spec, i)
		if err != nil {
			return nil, nil, err
		}
		seenKey := fmt.Sprintf("%s/%s", key, typ)
		collisionErr := rejectBackendCollision(variantRegistry[key], key, typ)
		if collisionErr != nil {
			return nil, nil, collisionErr
		}
		if prev, dup := seen[seenKey]; dup {
			return nil, nil, fmt.Errorf(
				"agent config: duplicate pool/backend %q: backends[%d] conflicts "+
					"with backends[%d]",
				seenKey, i, prev,
			)
		}
		seen[seenKey] = i
		b := newConfiguredBackend(spec, typ, configfsRoot, datasetRoot)
		if _, exists := registries[key]; !exists {
			registries[key] = b
		}
		if variantRegistry[key] == nil {
			variantRegistry[key] = make(map[agentv1.BackendType]backend.VolumeBackend)
		}
		variantRegistry[key][typ] = b
	}
	return registries, variantRegistry, nil
}

func backendTypeForSpec(spec pillarv1alpha1.BackendSpec, index int) (agentv1.BackendType, error) {
	switch {
	case spec.ZFS != nil && spec.ZFS.VolumeType == pillarv1alpha1.ZFSVolumeTypeDataset:
		return agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET, nil
	case spec.ZFS != nil:
		return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL, nil
	case spec.LVM != nil:
		return agentv1.BackendType_BACKEND_TYPE_LVM, nil
	default:
		return agentv1.BackendType_BACKEND_TYPE_UNSPECIFIED, fmt.Errorf(
			"agent config: backends[%d]: exactly one of lvm, zfs must be set", index,
		)
	}
}

func rejectBackendCollision(
	existing map[agentv1.BackendType]backend.VolumeBackend,
	key string,
	typ agentv1.BackendType,
) error {
	if len(existing) == 0 {
		return nil
	}
	if typ == agentv1.BackendType_BACKEND_TYPE_LVM {
		return fmt.Errorf("agent config: pool/VG name collision %q between LVM and another backend", key)
	}
	for existingType := range existing {
		if existingType == agentv1.BackendType_BACKEND_TYPE_LVM {
			return fmt.Errorf("agent config: pool/VG name collision %q between LVM and another backend", key)
		}
	}
	return nil
}

func newConfiguredBackend(
	spec pillarv1alpha1.BackendSpec,
	typ agentv1.BackendType,
	configfsRoot, datasetRoot string,
) backend.VolumeBackend {
	switch {
	case spec.ZFS != nil && typ == agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET:
		return zfs.NewDataset(spec.ZFS.Pool, spec.ZFS.ParentDataset, datasetRoot)
	case spec.ZFS != nil:
		return zfs.New(spec.ZFS.Pool, spec.ZFS.ParentDataset,
			zfs.WithConfigfsRoot(configfsRoot))
	case spec.LVM != nil:
		mode := lvm.ProvisionModeLinear
		if spec.LVM.ProvisioningMode == pillarv1alpha1.LVMProvisioningModeThin {
			mode = lvm.ProvisionModeThin
		}
		return lvm.New(spec.LVM.VolumeGroup, spec.LVM.ThinPool).WithMode(mode)
	default:
		panic("backend type derived without a configured backend")
	}
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

type configuredAgent struct {
	volumeBackends map[string]backend.VolumeBackend
	variants       map[string]map[agentv1.BackendType]backend.VolumeBackend
	nfsManager     *nfs.Manager
}

func configureAgent(configPath, configfsRoot, nfsBindAddress string) (configuredAgent, error) {
	specs, err := loadAgentConfig(configPath)
	if err != nil {
		return configuredAgent{}, err
	}
	volumeBackends, variants, err := buildVolumeBackendRegistry(
		specs, configfsRoot, agentDatasetRoot,
	)
	if err != nil {
		return configuredAgent{}, err
	}
	return configuredAgent{
		volumeBackends: volumeBackends,
		variants:       variants,
		nfsManager:     startNFSManager(specs, nfsBindAddress),
	}, nil
}

func startNFSManager(specs []pillarv1alpha1.BackendSpec, bindAddress string) *nfs.Manager {
	for _, spec := range specs {
		if spec.ZFS == nil || spec.ZFS.VolumeType != pillarv1alpha1.ZFSVolumeTypeDataset {
			continue
		}
		manager, err := nfs.NewManager(nfs.Config{
			StateDir:    agentNFSStateRoot,
			ExportRoot:  agentDatasetRoot,
			BindAddress: bindAddress,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "pillar-agent: WARNING: NFS unavailable: %v\n", err)
			return nil
		}
		startErr := manager.Start(context.Background())
		if startErr != nil {
			fmt.Fprintf(os.Stderr, "pillar-agent: WARNING: NFS unavailable: %v\n", startErr)
			closeNFSManager(manager)
			return nil
		}
		return manager
	}
	return nil
}

func closeNFSManager(manager *nfs.Manager) {
	if manager == nil {
		return
	}
	err := manager.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pillar-agent: NFS shutdown: %v\n", err)
	}
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
	nfsBindAddress := flag.String("nfs-bind-address", os.Getenv("PILLAR_AGENT_BIND_ADDRESS"),
		"numeric node address advertised by NFS (required for dataset backends)")
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

	runtime, err := configureAgent(*configPath, *cfgRoot, *nfsBindAddress)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	volumeBackends := runtime.volumeBackends
	variants := runtime.variants
	nfsManager := runtime.nfsManager
	opts := []agent.ServerOption{agent.WithExportRestoreGate(), agent.WithBackendVariants(variants)}
	if nfsManager != nil {
		opts = append(opts, agent.WithNFSManager(nfsManager))
	}
	srv := agent.NewServer(volumeBackends, *cfgRoot, opts...)
	serveAgent(srv, serveConfig{
		listenAddr: *listenAddr, metricsAddr: *metricsAddr, gracePeriod: *gracePeriod,
		tlsEnabled: tlsEnabled, tlsCert: *tlsCert, tlsKey: *tlsKey, tlsCA: *tlsCA,
		nfsManager: nfsManager,
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
	nfsManager  *nfs.Manager
}

// serveAgent sets up telemetry and the metrics endpoint, then serves srv
// over gRPC until the server stops.  It exits the process on failure.
func serveAgent(srv *agent.Server, cfg serveConfig) {
	version, _ := telemetry.BuildVersion()
	shutdownTelemetry, err := telemetry.Setup(context.Background(), telemetry.ComponentAgent, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: telemetry setup: %v\n", err)
		closeNFSManager(cfg.nfsManager)
		os.Exit(1)
	}
	// os.Exit skips defers, so every exit path below calls fail, which
	// shuts the metrics server down and flushes pending spans first.
	var metricsSrv *http.Server
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format, args...)
		closeNFSManager(cfg.nfsManager)
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

	jsonLog := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// Handlers that log outside a gRPC failure (e.g. the iSCSI handler's
	// discard notice) use the default logger; keep it in the same JSON form.
	slog.SetDefault(jsonLog)
	failureLog := telemetry.SlogFailureLogger(jsonLog)
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
	closeNFSManager(cfg.nfsManager)
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
