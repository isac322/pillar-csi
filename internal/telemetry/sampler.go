package telemetry

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// EnvSamplerArg is the standard OTel variable that carries the root sampling
// ratio. OTEL_TRACES_SAMPLER itself is ignored: [NewSampler] is always used.
const EnvSamplerArg = "OTEL_TRACES_SAMPLER_ARG"

// NewSampler returns ParentBased(root = dropParentlessClient(TraceIDRatioBased(ratio))).
//
// A span with a parent follows the parent's sampled flag. A root CLIENT span
// is always dropped, so background loops that dial the agent without an
// enclosing span (health poll, per-PVS resync) record nothing and inject
// sampled=0 into the agent. Any other root span is sampled at ratio.
func NewSampler(ratio float64) sdktrace.Sampler {
	return sdktrace.ParentBased(dropParentlessClient{root: sdktrace.TraceIDRatioBased(ratio)})
}

type dropParentlessClient struct {
	root sdktrace.Sampler
}

func (s dropParentlessClient) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	psc := trace.SpanContextFromContext(p.ParentContext)
	if p.Kind == trace.SpanKindClient && !psc.IsValid() {
		return sdktrace.SamplingResult{Decision: sdktrace.Drop, Tracestate: psc.TraceState()}
	}
	return s.root.ShouldSample(p)
}

func (s dropParentlessClient) Description() string {
	return "DropParentlessClient{" + s.root.Description() + "}"
}

// SamplerRatioFromEnv parses OTEL_TRACES_SAMPLER_ARG. Unset or blank means
// 1.0; a finite number is clamped to [0,1]; anything else is an error so a
// typo fails startup instead of silently tracing at a default rate.
func SamplerRatioFromEnv() (float64, error) {
	return parseSamplerRatio(os.Getenv(EnvSamplerArg))
}

func parseSamplerRatio(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 1.0, nil
	}
	r, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q: %w", EnvSamplerArg, raw, err)
	}
	if math.IsNaN(r) {
		return 0, fmt.Errorf("parse %s=%q: not a number", EnvSamplerArg, raw)
	}
	return min(1, max(0, r)), nil
}
