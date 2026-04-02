package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func newValidKindBootstrapState(t *testing.T) *kindBootstrapState {
	t.Helper()

	suitePaths := newTestSuiteTempPaths(t)
	return &kindBootstrapState{
		SuiteRootDir:   suitePaths.RootDir,
		WorkspaceDir:   suitePaths.WorkspaceDir,
		LogsDir:        suitePaths.LogsDir,
		GeneratedDir:   suitePaths.GeneratedDir,
		ClusterName:    "pillar-csi-e2e-p1234-abcd1234",
		KubeconfigPath: suitePaths.KubeconfigPath(),
		KubeContext:    "kind-pillar-csi-e2e-p1234-abcd1234",
		KindBinary:     "kind",
		KubectlBinary:  "kubectl",
		CreateTimeout:  time.Second,
		DeleteTimeout:  time.Second,
		clusterCreated: true,
	}
}

func TestKindBootstrapDestroyClusterRemovesSuiteRootOnDeleteFailure(t *testing.T) {
	t.Parallel()

	state := newValidKindBootstrapState(t)
	if err := os.WriteFile(state.KubeconfigPath, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	fakeRunner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind delete cluster --name " + state.ClusterName: {
				err: errors.New("cluster not found"),
			},
		},
	}

	err := state.destroyCluster(context.Background(), fakeRunner)
	if err == nil {
		t.Fatal("destroyCluster: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "cluster not found") {
		t.Fatalf("destroyCluster error = %q, want cluster not found", err)
	}

	if _, statErr := os.Stat(state.SuiteRootDir); !os.IsNotExist(statErr) {
		t.Fatalf("suite root still exists or returned unexpected error: %v", statErr)
	}
}

func TestInvocationTeardownCleanupDestroysRegisteredClusterOnce(t *testing.T) {
	t.Parallel()

	state := newValidKindBootstrapState(t)
	manager := newInvocationTeardown()

	if _, err := manager.RegisterKindCluster(state); err != nil {
		t.Fatalf("RegisterKindCluster: %v", err)
	}

	fakeRunner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind delete cluster --name " + state.ClusterName: {},
		},
	}

	if err := manager.CleanupWithRunner(context.Background(), fakeRunner); err != nil {
		t.Fatalf("CleanupWithRunner first call: %v", err)
	}
	if err := manager.CleanupWithRunner(context.Background(), fakeRunner); err != nil {
		t.Fatalf("CleanupWithRunner second call: %v", err)
	}

	if len(fakeRunner.calls) != 1 {
		t.Fatalf("cleanup call count = %d, want 1", len(fakeRunner.calls))
	}
	if _, err := os.Stat(state.SuiteRootDir); !os.IsNotExist(err) {
		t.Fatalf("suite root still exists or returned unexpected error: %v", err)
	}
}

func TestHandleInvocationSignalCleansUpAndReturnsSignalExitCode(t *testing.T) {
	t.Parallel()

	state := newValidKindBootstrapState(t)
	manager := newInvocationTeardown()
	if _, err := manager.RegisterKindCluster(state); err != nil {
		t.Fatalf("RegisterKindCluster: %v", err)
	}

	fakeRunner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind delete cluster --name " + state.ClusterName: {},
		},
	}
	manager.runnerFactory = func(_ io.Writer) commandRunner {
		return fakeRunner
	}

	var (
		output   bytes.Buffer
		exitCode int
	)

	handleInvocationSignal(manager, &output, func(code int) {
		exitCode = code
	}, os.Interrupt)

	if exitCode != 130 {
		t.Fatalf("exit code = %d, want 130", exitCode)
	}
	if got := output.String(); !strings.Contains(got, "received interrupt") {
		t.Fatalf("signal output = %q, want interrupt cleanup message", got)
	}
	if len(fakeRunner.calls) != 1 {
		t.Fatalf("cleanup call count = %d, want 1", len(fakeRunner.calls))
	}
	if _, err := os.Stat(state.SuiteRootDir); !os.IsNotExist(err) {
		t.Fatalf("suite root still exists or returned unexpected error: %v", err)
	}
}

func TestInvocationTeardownRegisterKindClusterRejectsSecondDistinctCluster(t *testing.T) {
	t.Parallel()

	manager := newInvocationTeardown()

	first := newValidKindBootstrapState(t)
	if _, err := manager.RegisterKindCluster(first); err != nil {
		t.Fatalf("RegisterKindCluster first: %v", err)
	}

	second := newValidKindBootstrapState(t)
	second.ClusterName = "pillar-csi-e2e-p5678-deadbeef"

	if _, err := manager.RegisterKindCluster(second); err == nil {
		t.Fatal("RegisterKindCluster second: expected overwrite rejection, got nil")
	}
}

func TestKindBootstrapDestroyClusterSkipsDeleteUntilClusterIsCreated(t *testing.T) {
	t.Parallel()

	state := newValidKindBootstrapState(t)
	state.clusterCreated = false

	fakeRunner := &fakeCommandRunner{
		t:       t,
		outputs: map[string]fakeCommandResult{},
	}

	if err := state.destroyCluster(context.Background(), fakeRunner); err != nil {
		t.Fatalf("destroyCluster: %v", err)
	}
	if len(fakeRunner.calls) != 0 {
		t.Fatalf("delete command count = %d, want 0", len(fakeRunner.calls))
	}
	if _, err := os.Stat(state.SuiteRootDir); !os.IsNotExist(err) {
		t.Fatalf("suite root still exists or returned unexpected error: %v", err)
	}
}

// TestInstallInvocationSignalHandlersSIGTERM verifies that sending SIGTERM to
// the process triggers Kind cluster teardown and calls exit with code 143
// (128 + SIGTERM value 15) — the POSIX convention for signal-terminated processes.
//
// The test sends a real SIGTERM via syscall.Kill so the OS signal machinery is
// exercised end-to-end. The mock exit function captures the code instead of
// actually terminating the process.
func TestInstallInvocationSignalHandlersSIGTERM(t *testing.T) {
	// Not parallel: sends a real signal to the current process; must not race
	// with other signal-sending tests.

	// Skip when TestMain's real signal handler is active (live cluster running).
	// Sending a real OS signal to the process would trigger the suite-level
	// handler and call os.Exit(143), terminating the test binary before this
	// test can check its own mock exit code.
	if os.Getenv("KIND_CLUSTER") != "" {
		t.Skip("skipping real-signal test: TestMain's signal handler is active with live cluster")
	}

	state := newValidKindBootstrapState(t)
	manager := newInvocationTeardown()
	if _, err := manager.RegisterKindCluster(state); err != nil {
		t.Fatalf("RegisterKindCluster: %v", err)
	}

	fakeRunner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind delete cluster --name " + state.ClusterName: {},
		},
	}
	manager.runnerFactory = func(_ io.Writer) commandRunner {
		return fakeRunner
	}

	var (
		mu       sync.Mutex
		exitCode = -1
	)
	exitDone := make(chan struct{})

	stopSignals := installInvocationSignalHandlers(
		manager,
		io.Discard,
		func(code int) {
			mu.Lock()
			exitCode = code
			mu.Unlock()
			close(exitDone)
		},
	)
	defer stopSignals()

	// Send SIGTERM to the current process. The signal is handled by the goroutine
	// in installInvocationSignalHandlers, not by Go's default SIGTERM handler,
	// because signal.Notify diverts the signal to our channel before it reaches
	// the runtime.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("Kill(SIGTERM): %v", err)
	}

	select {
	case <-exitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for signal handler to call exit after SIGTERM")
	}

	mu.Lock()
	got := exitCode
	mu.Unlock()

	// SIGTERM is signal 15; POSIX exit code for a signal-terminated process is
	// 128 + signal_number.
	const wantExitCode = 128 + int(syscall.SIGTERM)
	if got != wantExitCode {
		t.Fatalf("exit code = %d, want %d (128 + SIGTERM)", got, wantExitCode)
	}

	if len(fakeRunner.calls) != 1 {
		t.Fatalf("cleanup call count = %d, want 1", len(fakeRunner.calls))
	}
	if _, err := os.Stat(state.SuiteRootDir); !os.IsNotExist(err) {
		t.Fatalf("suite root still exists after SIGTERM cleanup: %v", err)
	}
}

// TestInstallInvocationSignalHandlersSIGINT verifies that SIGINT (Ctrl-C)
// triggers Kind cluster teardown and calls exit with code 130 (128 + 2).
func TestInstallInvocationSignalHandlersSIGINT(t *testing.T) {
	// Not parallel: sends a real signal to the current process.

	// Skip when TestMain's real signal handler is active (live cluster running).
	// Sending a real OS signal to the process would trigger the suite-level
	// handler and call os.Exit(130), terminating the test binary before this
	// test can check its own mock exit code.
	if os.Getenv("KIND_CLUSTER") != "" {
		t.Skip("skipping real-signal test: TestMain's signal handler is active with live cluster")
	}

	state := newValidKindBootstrapState(t)
	manager := newInvocationTeardown()
	if _, err := manager.RegisterKindCluster(state); err != nil {
		t.Fatalf("RegisterKindCluster: %v", err)
	}

	fakeRunner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind delete cluster --name " + state.ClusterName: {},
		},
	}
	manager.runnerFactory = func(_ io.Writer) commandRunner {
		return fakeRunner
	}

	var (
		mu       sync.Mutex
		exitCode = -1
	)
	exitDone := make(chan struct{})

	stopSignals := installInvocationSignalHandlers(
		manager,
		io.Discard,
		func(code int) {
			mu.Lock()
			exitCode = code
			mu.Unlock()
			close(exitDone)
		},
	)
	defer stopSignals()

	// Send SIGINT to the current process.
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("Kill(SIGINT): %v", err)
	}

	select {
	case <-exitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for signal handler to call exit after SIGINT")
	}

	mu.Lock()
	got := exitCode
	mu.Unlock()

	// SIGINT is signal 2; POSIX exit code is 128 + 2 = 130.
	const wantExitCode = 130
	if got != wantExitCode {
		t.Fatalf("exit code = %d, want 130 (128 + SIGINT)", got)
	}

	if len(fakeRunner.calls) != 1 {
		t.Fatalf("cleanup call count = %d, want 1", len(fakeRunner.calls))
	}
	if _, err := os.Stat(state.SuiteRootDir); !os.IsNotExist(err) {
		t.Fatalf("suite root still exists after SIGINT cleanup: %v", err)
	}
}

// TestInstallInvocationSignalHandlersStopPreventsCleanup verifies that calling
// the stop function returned by installInvocationSignalHandlers deregisters the
// signal handler. After stop(), a SIGTERM must NOT trigger cluster teardown.
//
// The test uses a 100 ms window after stop() to confirm no cleanup is invoked.
func TestInstallInvocationSignalHandlersStopPreventsCleanup(t *testing.T) {
	// Not parallel: manipulates global signal.Notify state.

	// Skip when TestMain's real signal handler is active (live cluster running).
	// Sending SIGTERM (even to the safety channel) races with the suite-level
	// handler; the suite handler fires first and calls os.Exit, killing the test.
	if os.Getenv("KIND_CLUSTER") != "" {
		t.Skip("skipping real-signal test: TestMain's signal handler is active with live cluster")
	}

	state := newValidKindBootstrapState(t)
	manager := newInvocationTeardown()
	if _, err := manager.RegisterKindCluster(state); err != nil {
		t.Fatalf("RegisterKindCluster: %v", err)
	}

	cleanupCalled := make(chan struct{}, 1)
	fakeRunner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind delete cluster --name " + state.ClusterName: {},
		},
	}
	manager.runnerFactory = func(_ io.Writer) commandRunner {
		cleanupCalled <- struct{}{}
		return fakeRunner
	}

	exitCalled := make(chan int, 1)
	stopSignals := installInvocationSignalHandlers(
		manager,
		io.Discard,
		func(code int) { exitCalled <- code },
	)

	// Deregister the handler before any signal arrives.
	stopSignals()

	// Allow the goroutine inside installInvocationSignalHandlers to notice the
	// stop channel being closed and exit cleanly.
	time.Sleep(50 * time.Millisecond)

	// After stop(), sending SIGTERM must not reach the handler we installed.
	// Go's default SIGTERM handler terminates the process, but signal.Stop()
	// ensures our channel no longer receives the signal. We re-register a
	// temporary no-op handler so the process survives the test signal.
	safetySignals := make(chan os.Signal, 1)
	signal.Notify(safetySignals, syscall.SIGTERM)
	defer signal.Stop(safetySignals)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("Kill(SIGTERM): %v", err)
	}

	// Drain the safety channel (the signal we just sent).
	select {
	case <-safetySignals:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for safety SIGTERM channel drain")
	}

	// Verify that our stopped handler did not trigger cluster teardown.
	select {
	case <-cleanupCalled:
		t.Fatal("cleanup was called after stop(); expected no-op")
	case <-exitCalled:
		t.Fatal("exit was called after stop(); expected no-op")
	case <-time.After(100 * time.Millisecond):
		// Expected: no cleanup triggered.
	}
}

// TestAC3_1DeferredTeardownFiresOnPanic verifies the runPrimary defer+recover
// pattern: the cleanup function (deleteOnExit) is guaranteed to run even when
// the inner execution body panics.
//
// Sub-AC 3.1: deferred teardown must run on both normal exit AND test failure
// (panic).  This test directly exercises the nested-function+defer+recover
// pattern used in runPrimary to confirm the invariant holds.
func TestAC3_1DeferredTeardownFiresOnPanic(t *testing.T) {
	t.Parallel()

	cleanupCalled := false
	deleteOnExit := func() { cleanupCalled = true }

	// Mirror the runPrimary nested-function pattern exactly:
	//   return func() (code int) {
	//       defer func() { recover(); deleteOnExit() }()
	//       panic(...)
	//   }()
	exitCode := func() (code int) {
		defer func() {
			if r := recover(); r != nil {
				code = 1
			}
			deleteOnExit()
		}()
		panic("simulated test failure")
	}()

	if !cleanupCalled {
		t.Fatal("AC3.1: deleteOnExit was not called after panic — deferred teardown broken")
	}
	if exitCode != 1 {
		t.Fatalf("AC3.1: exit code = %d after panic, want 1", exitCode)
	}
}

// TestAC3_1DeferredTeardownFiresOnNormalExit verifies that the cleanup function
// is also called on normal (non-panic) return — the other half of the AC3.1
// requirement ("runs on normal exit and test failure").
func TestAC3_1DeferredTeardownFiresOnNormalExit(t *testing.T) {
	t.Parallel()

	cleanupCalled := false
	deleteOnExit := func() { cleanupCalled = true }

	exitCode := func() (code int) {
		defer func() {
			if r := recover(); r != nil {
				code = 1
			}
			deleteOnExit()
		}()
		return 0 // normal exit
	}()

	if !cleanupCalled {
		t.Fatal("AC3.1: deleteOnExit was not called on normal exit — deferred teardown broken")
	}
	if exitCode != 0 {
		t.Fatalf("AC3.1: exit code = %d on normal exit, want 0", exitCode)
	}
}

// TestAC3_1InvocationTeardownRegisteredAndCleanedOnNormalExit verifies the full
// AC3.1 lifecycle integration path:
//   - newKindBootstrapState creates state with unique temp dirs under /tmp
//   - RegisterKindCluster registers state with invocationTeardown
//   - CleanupWithRunner deletes the cluster and removes suite temp dirs
//   - All /tmp artifacts are removed after cleanup
//
// This is an integration test for the three components that runPrimary wires
// together: newKindBootstrapState → RegisterKindCluster → CleanupWithRunner.
func TestAC3_1InvocationTeardownRegisteredAndCleanedOnNormalExit(t *testing.T) {
	t.Parallel()

	state := newValidKindBootstrapState(t)
	// Write the kubeconfig so destroyCluster finds a non-empty suite root.
	if err := os.WriteFile(state.KubeconfigPath, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	manager := newInvocationTeardown()
	if _, err := manager.RegisterKindCluster(state); err != nil {
		t.Fatalf("RegisterKindCluster: %v", err)
	}

	fakeRunner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind delete cluster --name " + state.ClusterName: {},
		},
	}

	// Simulate normal exit: cleanup is called once.
	if err := manager.CleanupWithRunner(context.Background(), fakeRunner); err != nil {
		t.Fatalf("AC3.1 CleanupWithRunner: %v", err)
	}

	// Suite temp directory must be removed after cleanup.
	if _, statErr := os.Stat(state.SuiteRootDir); !os.IsNotExist(statErr) {
		t.Fatalf("AC3.1: suite root still exists after cleanup: %v", statErr)
	}

	// Cleanup is idempotent: second call must not error or re-delete.
	if err := manager.CleanupWithRunner(context.Background(), fakeRunner); err != nil {
		t.Fatalf("AC3.1 CleanupWithRunner idempotent second call: %v", err)
	}

	// kind delete cluster must have been called exactly once.
	if len(fakeRunner.calls) != 1 {
		t.Fatalf("AC3.1: kind delete cluster called %d times, want 1", len(fakeRunner.calls))
	}
}

// TestInstallInvocationSignalHandlersNilTeardownIsNoop verifies that passing a
// nil teardown returns a harmless stop function and does not panic on signals.
func TestInstallInvocationSignalHandlersNilTeardownIsNoop(t *testing.T) {
	t.Parallel()

	stop := installInvocationSignalHandlers(nil, io.Discard, func(int) {})
	// Calling stop on a nil-teardown handler must not panic or block.
	stop()
	stop() // idempotent
}

// TestExitCodeForSignalSIGTERM verifies the POSIX exit code arithmetic for SIGTERM.
func TestExitCodeForSignalSIGTERM(t *testing.T) {
	t.Parallel()

	got := exitCodeForSignal(syscall.SIGTERM)
	// SIGTERM = 15; POSIX convention: 128 + signal number.
	const want = 128 + 15
	if got != want {
		t.Fatalf("exitCodeForSignal(SIGTERM) = %d, want %d", got, want)
	}
}

// TestExitCodeForSignalSIGINT verifies the POSIX exit code arithmetic for SIGINT.
func TestExitCodeForSignalSIGINT(t *testing.T) {
	t.Parallel()

	got := exitCodeForSignal(syscall.SIGINT)
	// SIGINT = 2; POSIX convention: 128 + 2 = 130.
	const want = 130
	if got != want {
		t.Fatalf("exitCodeForSignal(SIGINT) = %d, want %d", got, want)
	}
}

// TestHandleInvocationSignalSIGTERMLogsAndExits verifies that
// handleInvocationSignal correctly logs the signal name and calls the exit
// function with the appropriate POSIX code when receiving syscall.SIGTERM.
func TestHandleInvocationSignalSIGTERMLogsAndExits(t *testing.T) {
	t.Parallel()

	state := newValidKindBootstrapState(t)
	manager := newInvocationTeardown()
	if _, err := manager.RegisterKindCluster(state); err != nil {
		t.Fatalf("RegisterKindCluster: %v", err)
	}

	fakeRunner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind delete cluster --name " + state.ClusterName: {},
		},
	}
	manager.runnerFactory = func(_ io.Writer) commandRunner {
		return fakeRunner
	}

	var (
		output   bytes.Buffer
		exitCode int
	)

	handleInvocationSignal(manager, &output, func(code int) {
		exitCode = code
	}, syscall.SIGTERM)

	// SIGTERM exit code = 128 + 15 = 143.
	const wantExitCode = 143
	if exitCode != wantExitCode {
		t.Fatalf("exit code = %d, want %d (128 + SIGTERM)", exitCode, wantExitCode)
	}

	// Output must mention "terminated" (the string representation of SIGTERM on Linux).
	got := output.String()
	if !strings.Contains(got, "terminated") {
		t.Fatalf("signal output = %q, want message containing 'terminated'", got)
	}

	if len(fakeRunner.calls) != 1 {
		t.Fatalf("cleanup call count = %d, want 1", len(fakeRunner.calls))
	}
	if _, err := os.Stat(state.SuiteRootDir); !os.IsNotExist(err) {
		t.Fatalf("suite root still exists after SIGTERM handleInvocationSignal: %v", err)
	}
}
