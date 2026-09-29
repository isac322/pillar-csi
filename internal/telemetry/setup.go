package telemetry

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope of every manual pillar-csi span.
const ScopeName = "github.com/isac322/pillar-csi"

// Components, used for service.name (pillar-csi-<component>) and the
// component label of pillar_csi_build_info.
const (
	ComponentController = "controller"
	ComponentAgent      = "agent"
	ComponentNode       = "node"
)

// OTLP environment variables read by [Setup]. The exporter itself reads the
// rest of the OTEL_EXPORTER_OTLP_* family (insecure, headers, timeout).
const (
	EnvOTLPEndpoint       = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvOTLPTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	EnvOTLPProtocol       = "OTEL_EXPORTER_OTLP_PROTOCOL"
	EnvOTLPTracesProtocol = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
)

var serviceVersion atomic.Pointer[string]

// Tracer returns the pillar-csi tracer from the global TracerProvider, tagged
// with the version passed to [Setup]. Call it at span start rather than
// caching the result before Setup runs.
func Tracer() trace.Tracer {
	var v string
	if p := serviceVersion.Load(); p != nil {
		v = *p
	}
	return otel.GetTracerProvider().Tracer(ScopeName, trace.WithInstrumentationVersion(v))
}

// TracingEnabled reports whether the environment configures an OTLP traces
// endpoint, i.e. whether [Setup] installs an SDK TracerProvider.
func TracingEnabled() bool {
	return strings.TrimSpace(os.Getenv(EnvOTLPEndpoint)) != "" ||
		strings.TrimSpace(os.Getenv(EnvOTLPTracesEndpoint)) != ""
}

// Setup configures tracing for one binary. Pass one of the Component*
// constants as component and the binary version (see [BuildVersion]).
//
// It always installs the W3C TraceContext propagator (no baggage). It
// installs an SDK TracerProvider only when [TracingEnabled]; otherwise the
// global no-op provider stays and the returned shutdown does nothing. With
// tracing enabled it fails when an OTLP protocol other than grpc is requested
// or OTEL_TRACES_SAMPLER_ARG does not parse.
//
// The returned shutdown flushes pending spans; call it once before exit,
// including on error paths that end in os.Exit.
func Setup(ctx context.Context, component, version string) (shutdown func(context.Context) error, err error) {
	serviceVersion.Store(&version)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	noop := func(context.Context) error { return nil }
	if !TracingEnabled() {
		return noop, nil
	}

	for _, key := range []string{EnvOTLPProtocol, EnvOTLPTracesProtocol} {
		if p := strings.TrimSpace(os.Getenv(key)); p != "" && p != "grpc" {
			return noop, fmt.Errorf("telemetry setup: %s=%q: only \"grpc\" is supported", key, p)
		}
	}

	ratio, err := SamplerRatioFromEnv()
	if err != nil {
		return noop, fmt.Errorf("telemetry setup: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("pillar-csi-"+component),
			semconv.ServiceVersion(version),
		),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	if err != nil {
		return noop, fmt.Errorf("telemetry setup: build resource: %w", err)
	}

	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return noop, fmt.Errorf("telemetry setup: create OTLP gRPC trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(NewSampler(ratio)),
	)
	otel.SetTracerProvider(tp)

	return func(shutdownCtx context.Context) error {
		shutdownErr := tp.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			return fmt.Errorf("telemetry shutdown: %w", shutdownErr)
		}
		return nil
	}, nil
}
