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

package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/isac322/pillar-csi/internal/telemetry"
)

// metricsDisabled is the --metrics-bind-address value that disables the
// metrics endpoint (the controller's convention).
const metricsDisabled = "0"

// metricsReadHeaderTimeout bounds slow-header clients on the plaintext
// metrics endpoint.
const metricsReadHeaderTimeout = 10 * time.Second

// newNodeMetricsRegistry builds the node's own registry: Go and process
// collectors (M19), pillar_csi_build_info (M18), the exec histogram (M9) and
// the NVMe-oF controller collector (M17) over sysfsRoot.
func newNodeMetricsRegistry(version, sysfsRoot string) (*prometheus.Registry, error) {
	reg := prometheus.NewRegistry()
	for _, c := range []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		telemetry.BuildInfoCollector(telemetry.ComponentNode, version),
		newNVMeoFControllersCollector(sysfsRoot),
	} {
		err := reg.Register(c)
		if err != nil {
			return nil, fmt.Errorf("register node metrics collector: %w", err)
		}
	}
	err := telemetry.RegisterExecMetrics(reg)
	if err != nil {
		return nil, fmt.Errorf("register node metrics: %w", err)
	}
	return reg, nil
}

// startMetricsServer serves reg on addr at /metrics with OpenMetrics
// (exemplars) enabled.  It returns nil when addr is metricsDisabled or
// empty.  The listener is opened before returning so a bad address fails
// startup; a later serve failure is logged to stderr.
func startMetricsServer(addr string, reg *prometheus.Registry) (*http.Server, error) {
	if addr == metricsDisabled || addr == "" {
		return nil, nil //nolint:nilnil // a disabled endpoint has no server and is not an error
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics server: listen tcp %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
		ErrorHandling:     promhttp.ContinueOnError,
		ErrorLog:          log.New(os.Stderr, "pillar-node: metrics: ", 0),
	}))
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: metricsReadHeaderTimeout,
	}
	go func() {
		serveErr := srv.Serve(lis)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "pillar-node: metrics server on %s: %v\n", addr, serveErr)
		}
	}()
	return srv, nil
}
