package telemetry

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// T2: the sampler keeps root SERVER/INTERNAL spans at the ratio, drops
// parentless CLIENT spans, and otherwise follows the parent.
func TestSampler(t *testing.T) {
	sampledParent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{1},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	unsampledParent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{2},
		SpanID:  trace.SpanID{2},
		Remote:  true,
	})

	tests := []struct {
		name   string
		ratio  float64
		parent trace.SpanContext
		kind   trace.SpanKind
		want   bool
	}{
		{"parentless client is dropped", 1, trace.SpanContext{}, trace.SpanKindClient, false},
		{"parentless server is sampled at r=1", 1, trace.SpanContext{}, trace.SpanKindServer, true},
		{"parentless internal is sampled at r=1", 1, trace.SpanContext{}, trace.SpanKindInternal, true},
		{"r=0 drops a server root", 0, trace.SpanContext{}, trace.SpanKindServer, false},
		{"r=0 drops an internal root", 0, trace.SpanContext{}, trace.SpanKindInternal, false},
		{"child of a sampled parent is sampled even at r=0", 0, sampledParent, trace.SpanKindServer, true},
		{"client child of a sampled parent is sampled", 0, sampledParent, trace.SpanKindClient, true},
		{"child of an unsampled remote parent is dropped", 1, unsampledParent, trace.SpanKindServer, false},
		{"client child of an unsampled remote parent is dropped", 1, unsampledParent, trace.SpanKindClient, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(NewSampler(tc.ratio)))
			t.Cleanup(func() {
				if err := tp.Shutdown(context.Background()); err != nil {
					t.Errorf("TracerProvider.Shutdown: %v", err)
				}
			})

			ctx := context.Background()
			if tc.parent.IsValid() {
				ctx = trace.ContextWithRemoteSpanContext(ctx, tc.parent)
			}
			_, span := tp.Tracer("test").Start(ctx, "span", trace.WithSpanKind(tc.kind))
			defer span.End()

			if got := span.SpanContext().IsSampled(); got != tc.want {
				t.Fatalf("sampled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSamplerRatioFromEnv(t *testing.T) {
	tests := []struct {
		raw     string
		want    float64
		wantErr bool
	}{
		{raw: "", want: 1},
		{raw: "0.25", want: 0.25},
		{raw: "0", want: 0},
		{raw: "1.5", want: 1},
		{raw: "-0.5", want: 0},
		{raw: "abc", wantErr: true},
		{raw: "NaN", wantErr: true},
		{raw: "10%", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv(EnvSamplerArg, tc.raw)
			got, err := SamplerRatioFromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SamplerRatioFromEnv() = %v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SamplerRatioFromEnv() error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("SamplerRatioFromEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}

// An unparsable ratio fails Setup when tracing is enabled instead of
// silently falling back to a default.
func TestSetupRejectsInvalidSamplerArg(t *testing.T) {
	t.Setenv(EnvOTLPEndpoint, "http://127.0.0.1:4317")
	t.Setenv(EnvSamplerArg, "half")
	if _, err := Setup(context.Background(), ComponentAgent, "test"); err == nil {
		t.Fatal("Setup() succeeded with an invalid OTEL_TRACES_SAMPLER_ARG")
	}
}
