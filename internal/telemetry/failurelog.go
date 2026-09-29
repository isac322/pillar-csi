package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Failure log line (L1) message and field keys.
const (
	FailureLogMessage = "rpc failed"

	LogKeyGRPCMethod      = "grpc.method"
	LogKeyGRPCCode        = "grpc.code"
	LogKeyDurationSeconds = "duration_seconds"
	LogKeyError           = "error"
	LogKeyTraceID         = "trace_id"
	LogKeySpanID          = "span_id"
)

// RPCFailure is one failed unary RPC, as logged by
// [UnaryServerFailureInterceptor]. Empty string fields are omitted.
type RPCFailure struct {
	FullMethod    string
	Code          grpccodes.Code
	Duration      time.Duration
	Message       string // the gRPC status message
	VolumeID      string // pillar_csi.volume.id (CSI requests)
	PVName        string // pillar_csi.pv.name (CSI CreateVolume, agent requests)
	FenceDecision string // pillar_csi.fence.decision (agent, when recorded)
	TraceID       string // only when the server span is sampled
	SpanID        string // only when the server span is sampled
}

// Level is ERROR for server-side codes (Unknown, DeadlineExceeded,
// Unimplemented, Internal, Unavailable, DataLoss) and WARN for every other
// non-OK code.
func (f RPCFailure) Level() slog.Level {
	switch f.Code {
	case grpccodes.Unknown, grpccodes.DeadlineExceeded, grpccodes.Unimplemented,
		grpccodes.Internal, grpccodes.Unavailable, grpccodes.DataLoss:
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}

type logField struct {
	key string
	val any
}

// logFields returns the fields of the line except msg and error, omitting
// empty optional values.
func (f RPCFailure) logFields() []logField {
	fields := []logField{
		{LogKeyGRPCMethod, f.FullMethod},
		{LogKeyGRPCCode, f.Code.String()},
		{LogKeyDurationSeconds, f.Duration.Seconds()},
	}
	for _, p := range []struct{ k, v string }{
		{string(KeyVolumeID), f.VolumeID},
		{string(KeyPVName), f.PVName},
		{string(KeyFenceDecision), f.FenceDecision},
		{LogKeyTraceID, f.TraceID},
		{LogKeySpanID, f.SpanID},
	} {
		if p.v != "" {
			fields = append(fields, logField{p.k, p.v})
		}
	}
	return fields
}

// keyValues flattens logFields into slog/logr key-value pairs.
func (f RPCFailure) keyValues() []any {
	fields := f.logFields()
	kv := make([]any, 0, 2*len(fields)+2)
	for _, fl := range fields {
		kv = append(kv, fl.key, fl.val)
	}
	return kv
}

// FailureLogger writes one L1 line per failed RPC. Use [SlogFailureLogger]
// (agent, node) or [LogrFailureLogger] (controller).
type FailureLogger interface {
	LogRPCFailure(ctx context.Context, f RPCFailure)
}

// SlogFailureLogger logs through l; agent and node pass a JSON handler on
// stderr.
func SlogFailureLogger(l *slog.Logger) FailureLogger { return slogFailureLogger{l} }

type slogFailureLogger struct{ l *slog.Logger }

func (s slogFailureLogger) LogRPCFailure(ctx context.Context, f RPCFailure) {
	s.l.Log(ctx, f.Level(), FailureLogMessage, append(f.keyValues(), LogKeyError, f.Message)...)
}

// LogrFailureLogger logs through l. ERROR lines use logr's Error. Since logr
// has no warning level, WARN lines go through the underlying zap logger's
// Warn when l is zapr-backed (controller-runtime's zap logger) and fall back
// to Info otherwise.
func LogrFailureLogger(l logr.Logger) FailureLogger { return logrFailureLogger{l} }

type logrFailureLogger struct{ l logr.Logger }

func (g logrFailureLogger) LogRPCFailure(_ context.Context, f RPCFailure) {
	if f.Level() >= slog.LevelError {
		g.l.Error(errors.New(f.Message), FailureLogMessage, f.keyValues()...)
		return
	}
	if u, ok := g.l.GetSink().(zapr.Underlier); ok {
		logFields := f.logFields()
		fields := make([]zap.Field, 0, len(logFields)+1)
		for _, fl := range logFields {
			fields = append(fields, zap.Any(fl.key, fl.val))
		}
		fields = append(fields, zap.String(LogKeyError, f.Message))
		u.GetUnderlying().Warn(FailureLogMessage, fields...)
		return
	}
	g.l.Info(FailureLogMessage, append(f.keyValues(), LogKeyError, f.Message)...)
}

// UnaryServerFailureInterceptor logs one line per failed unary RPC through
// log (no success lines) and sets error.type on the server span to the
// canonical code (e.g. FAILED_PRECONDITION) for every non-OK code.
//
// Volume fields come from the request: GetVolumeId() is logged as
// pillar_csi.volume.id on CSI servers, and CSI CreateVolume logs GetName() as
// pillar_csi.pv.name. Agent requests carry an agent volume ID "<pool>/<pv>",
// logged as pillar_csi.pv.name. A decision passed to [RecordFenceDecision]
// during the call is logged as pillar_csi.fence.decision.
//
// The otelgrpc stats handler starts the server span before interceptors run,
// so the span is already in ctx.
func UnaryServerFailureInterceptor(log FailureLogger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		notes := &failureNotes{}
		start := time.Now()
		resp, err := handler(context.WithValue(ctx, failureNotesKey{}, notes), req)
		if err == nil {
			return resp, nil
		}
		st := status.Convert(err)
		if st.Code() == grpccodes.OK {
			return resp, err
		}

		span := trace.SpanFromContext(ctx)
		span.SetAttributes(semconv.ErrorTypeKey.String(CanonicalCode(st.Code())))

		f := RPCFailure{
			FullMethod:    info.FullMethod,
			Code:          st.Code(),
			Duration:      time.Since(start),
			Message:       st.Message(),
			FenceDecision: notes.fenceDecision(),
		}
		f.VolumeID, f.PVName = requestVolumeFields(info.FullMethod, req)
		if sc := span.SpanContext(); sc.IsSampled() {
			f.TraceID = sc.TraceID().String()
			f.SpanID = sc.SpanID().String()
		}
		log.LogRPCFailure(ctx, f)
		return resp, err
	}
}

func requestVolumeFields(fullMethod string, req any) (volumeID, pvName string) {
	var id string
	if r, ok := req.(interface{ GetVolumeId() string }); ok {
		id = r.GetVolumeId()
	}
	if strings.HasPrefix(fullMethod, agentServicePrefix) {
		_, pv := splitAgentVolumeID(id)
		return "", pv
	}
	if fullMethod == "/csi.v1.Controller/CreateVolume" {
		if r, ok := req.(interface{ GetName() string }); ok {
			pvName = r.GetName()
		}
	}
	return id, pvName
}

type failureNotesKey struct{}

type failureNotes struct {
	mu       sync.Mutex
	decision string
}

func (n *failureNotes) fenceDecision() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.decision
}

// RecordFenceDecision records a fencing admit/recheck outcome for the RPC in
// ctx: pillar_csi.fence.op and pillar_csi.fence.decision on the current span
// and the decision on the RPC's failure log line. Both values are mapped
// through [FenceOpLabel] / [FenceDecisionLabel]. The last call wins. The M7
// counter is the caller's to increment.
func RecordFenceDecision(ctx context.Context, op, decision string) {
	op, decision = FenceOpLabel(op), FenceDecisionLabel(decision)
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(KeyFenceOp.String(op), KeyFenceDecision.String(decision))
	}
	if n, ok := ctx.Value(failureNotesKey{}).(*failureNotes); ok {
		n.mu.Lock()
		n.decision = decision
		n.mu.Unlock()
	}
}
