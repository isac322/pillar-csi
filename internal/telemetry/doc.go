// Package telemetry holds the tracing and metrics plumbing shared by the
// pillar-csi controller, agent and node binaries.
//
// Tracing is off unless an OTLP endpoint is configured (see [Setup]). All
// spans use the global TracerProvider and the W3C trace-context propagator, so
// the otelgrpc stats handlers built here ([ControllerServerHandler],
// [NodeServerHandler], [AgentServerHandler], [AgentClientHandler]) and the
// manual spans started through [Tracer] share one pipeline.
//
// Metric label values produced by this package come from closed sets: an
// unknown input maps to "other" and never to the raw value, so a new errno,
// executable or fencing branch cannot create unbounded series.
package telemetry
