package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// execOutputTailLimit caps pillar_csi.exec.output_tail.
const execOutputTailLimit = 1024

// execDuration is M9, pillar_csi_exec_duration_seconds. It is observed by
// every [Exec] whether or not it is registered; [RegisterExecMetrics] exposes
// it on the agent and node registries.
var execDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name: "pillar_csi_exec_duration_seconds",
	Help: "Duration of external commands (zfs, lvm, dmsetup, mkfs, resize) run by pillar-csi, " +
		"by executable, subcommand and result.",
	Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
}, []string{"executable", "subcommand", "result"})

// RegisterExecMetrics registers pillar_csi_exec_duration_seconds (M9).
func RegisterExecMetrics(reg prometheus.Registerer) error {
	err := reg.Register(execDuration)
	if err != nil {
		return fmt.Errorf("register pillar_csi_exec_duration_seconds: %w", err)
	}
	return nil
}

// Exec observes one external command: the M9 histogram always, and an SP8
// "exec <executable>[ <subcommand>]" span when ctx already carries a valid
// span context (so background execs outside a traced RPC create no root).
// Start it with [StartExec] right before running the command and finish it
// with [Exec.End] using the command's CombinedOutput result.
type Exec struct {
	ctx        context.Context
	start      time.Time
	name       string
	args       []string
	executable string
	subcommand string
	span       trace.Span // nil when no span was started
}

// StartExec starts observing the command name (a bare name or a path) with
// argv args (excluding argv[0]). Call [Exec.End] exactly once with the
// command's combined output and error.
func StartExec(ctx context.Context, name string, args ...string) *Exec {
	e := &Exec{
		ctx:        ctx,
		name:       name,
		args:       args,
		executable: ExecExecutableLabel(name),
	}
	e.subcommand = ExecSubcommandLabel(e.executable, args)

	if trace.SpanContextFromContext(ctx).IsValid() {
		spanName := "exec " + e.executable
		attrs := []attribute.KeyValue{semconv.ProcessExecutableName(filepath.Base(name))}
		if e.subcommand != "" {
			spanName += " " + e.subcommand
			attrs = append(attrs, KeyExecSubcommand.String(e.subcommand))
		}
		e.ctx, e.span = Tracer().Start(ctx, spanName,
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attrs...),
		)
	}
	e.start = time.Now()
	return e
}

// End records the result. Pass the command's CombinedOutput (may be nil) as
// output and the error returned by running it, unwrapped or wrapped, as err.
func (e *Exec) End(output []byte, err error) {
	elapsed := time.Since(e.start).Seconds()
	result, errorType, exitCode := e.classify(err)

	observeWithExemplar(e.ctx,
		execDuration.WithLabelValues(e.executable, e.subcommand, result), elapsed)

	if e.span == nil {
		return
	}
	defer e.span.End()
	if exitCode >= 0 {
		e.span.SetAttributes(semconv.ProcessExitCode(exitCode))
	}
	if err == nil {
		return
	}
	if _, ok := execRecordsArgs[e.executable]; ok {
		argv := make([]string, 0, len(e.args)+1)
		argv = append(argv, validUTF8(e.name))
		for _, a := range e.args {
			argv = append(argv, validUTF8(a))
		}
		e.span.SetAttributes(semconv.ProcessCommandArgs(argv...))
	}
	if len(output) > 0 {
		e.span.SetAttributes(KeyExecOutputTail.String(tailUTF8(output, execOutputTailLimit)))
	}
	SetSpanError(e.span, err, errorType)
}

// exitStatuser is k8s.io/utils/exec.ExitError, used by the mount-utils
// executor that runs mkfs.
type exitStatuser interface {
	ExitStatus() int
}

// classify returns the M9 result, the SP8 error.type, and the exit code
// (-1 when the process did not exit normally or never started).
func (e *Exec) classify(err error) (result, errorType string, exitCode int) {
	if err == nil {
		return ExecResultOK, "", 0
	}
	ctxErr := e.ctx.Err()
	if ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return ExecResultCanceled, "deadline_exceeded", -1
		}
		return ExecResultCanceled, "context_canceled", -1
	}
	var exitErr *exec.ExitError
	var statusErr exitStatuser
	switch {
	case errors.As(err, &exitErr):
		exitCode = exitErr.ExitCode()
	case errors.As(err, &statusErr):
		exitCode = statusErr.ExitStatus()
	default:
		return ExecResultStartError, "start_error", -1
	}
	if exitCode < 0 {
		// Killed by a signal: it ran but did not exit with a code.
		return ExecResultExitError, ErrorTypeOther, -1
	}
	return ExecResultExitError, "exit_" + strconv.Itoa(exitCode), exitCode
}
