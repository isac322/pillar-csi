package telemetry

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sys/unix"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Error type values shared by several spans.
const (
	ErrorTypeTimeout = "timeout"
	ErrorTypeOther   = "_OTHER"
)

// spanStatusMessageLimit caps the INTERNAL span status description.
const spanStatusMessageLimit = 512

// canonicalCodes maps gRPC codes to their canonical upper-case names.
var canonicalCodes = map[grpccodes.Code]string{
	grpccodes.OK:                 "OK",
	grpccodes.Canceled:           "CANCELLED", //nolint:misspell // canonical gRPC code name
	grpccodes.Unknown:            "UNKNOWN",
	grpccodes.InvalidArgument:    "INVALID_ARGUMENT",
	grpccodes.DeadlineExceeded:   "DEADLINE_EXCEEDED",
	grpccodes.NotFound:           "NOT_FOUND",
	grpccodes.AlreadyExists:      "ALREADY_EXISTS",
	grpccodes.PermissionDenied:   "PERMISSION_DENIED",
	grpccodes.ResourceExhausted:  "RESOURCE_EXHAUSTED",
	grpccodes.FailedPrecondition: "FAILED_PRECONDITION",
	grpccodes.Aborted:            "ABORTED",
	grpccodes.OutOfRange:         "OUT_OF_RANGE",
	grpccodes.Unimplemented:      "UNIMPLEMENTED",
	grpccodes.Internal:           "INTERNAL",
	grpccodes.Unavailable:        "UNAVAILABLE",
	grpccodes.DataLoss:           "DATA_LOSS",
	grpccodes.Unauthenticated:    "UNAUTHENTICATED",
}

// CanonicalCode returns the canonical upper-case gRPC code name
// (e.g. FAILED_PRECONDITION), the format otelgrpc uses for
// rpc.response.status_code. Unknown numeric codes render as CODE(<n>).
func CanonicalCode(c grpccodes.Code) string {
	if name, ok := canonicalCodes[c]; ok {
		return name
	}
	return "CODE(" + strconv.FormatUint(uint64(c), 10) + ")"
}

// ErrorType classifies err for error.type on an INTERNAL span when no
// span-specific value applies: the canonical code of a gRPC status error,
// else the errno name of a wrapped syscall.Errno, else "timeout" for
// context.DeadlineExceeded, else "_OTHER". A nil error returns "".
func ErrorType(err error) string {
	if err == nil {
		return ""
	}
	if s, ok := status.FromError(err); ok {
		return CanonicalCode(s.Code())
	}
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		if name := unix.ErrnoName(errno); name != "" {
			return name
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorTypeTimeout
	}
	return ErrorTypeOther
}

// SetSpanError applies the INTERNAL span error rule: status Error with the
// message truncated to 512 bytes, and error.type set to errorType, or to
// [ErrorType](err) when errorType is "". It does nothing for a nil err or a
// non-recording span.
func SetSpanError(span trace.Span, err error, errorType string) {
	if err == nil || !span.IsRecording() {
		return
	}
	if errorType == "" {
		errorType = ErrorType(err)
	}
	span.SetStatus(codes.Error, truncateUTF8(err.Error(), spanStatusMessageLimit))
	span.SetAttributes(semconv.ErrorTypeKey.String(errorType))
}

// truncateUTF8 returns the last-rune-safe prefix of s of at most limit bytes,
// with invalid UTF-8 replaced so OTLP (protobuf strings) accepts it.
func truncateUTF8(s string, limit int) string {
	s = validUTF8(s)
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// tailUTF8 returns the rune-safe suffix of b of at most limit bytes, with
// invalid UTF-8 replaced.
func tailUTF8(b []byte, limit int) string {
	if len(b) > limit {
		start := len(b) - limit
		for start < len(b) && !utf8.RuneStart(b[start]) {
			start++
		}
		b = b[start:]
	}
	return validUTF8(string(b))
}

// validUTF8 replaces invalid UTF-8 sequences, which OTLP's protobuf strings
// reject.
func validUTF8(s string) string {
	return strings.ToValidUTF8(s, "\uFFFD")
}
