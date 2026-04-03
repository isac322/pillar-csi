package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type runnerFactory func(io.Writer) commandRunner

// invocationTeardown tracks invocation-scoped resources that must be released
// even when Ginkgo suite teardown is skipped.
//
// Cleanup ordering (Sub-AC 5.2):
//  1. backend.teardown — destroys ZFS pools / LVM VGs inside the Kind container
//     while it is still alive.
//  2. kind.destroyCluster — deletes the Kind cluster and removes suite temp dirs.
//
// This ordering ensures that explicit ZFS/LVM teardown commands run against a
// live container rather than a deleted one.
type invocationTeardown struct {
	mu            sync.Mutex
	kind          *kindBootstrapState
	backend       *suiteBackendState // AC5.2: torn down before kind cluster
	runnerFactory runnerFactory
}

func newInvocationTeardown() *invocationTeardown {
	return &invocationTeardown{
		runnerFactory: func(output io.Writer) commandRunner {
			return execCommandRunner{Output: output}
		},
	}
}

var suiteInvocationTeardown = newInvocationTeardown()

// RegisterBackend registers the provisioned backend state so that Cleanup
// destroys ZFS pools and LVM VGs before the Kind cluster is deleted.
//
// Calling RegisterBackend with a nil state is a safe no-op (returns nil, nil).
// Registering a second non-nil state returns an error — only one backend state
// is tracked per invocation.
func (t *invocationTeardown) RegisterBackend(state *suiteBackendState) error {
	if state == nil {
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.backend != nil {
		return fmt.Errorf(
			"[AC5.2] backend already registered for invocation (container=%s)",
			t.backend.NodeContainer,
		)
	}
	t.backend = state
	return nil
}

func (t *invocationTeardown) RegisterKindCluster(state *kindBootstrapState) (*kindBootstrapState, error) {
	if state == nil {
		return nil, errors.New("kind bootstrap state is nil")
	}
	if err := state.validate(); err != nil {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.kind != nil {
		if sameKindClusterIdentity(t.kind, state) {
			return t.kind, nil
		}
		return nil, fmt.Errorf(
			"kind cluster already registered for invocation: cluster=%s suiteRoot=%s",
			t.kind.ClusterName,
			t.kind.SuiteRootDir,
		)
	}

	clone := *state
	t.kind = &clone
	return t.kind, nil
}

// Cleanup releases all invocation-scoped resources in the correct order:
//
//  1. Backend teardown (ZFS pools, LVM VGs) — runs while the Kind container
//     is still alive so that zpool/vg destroy commands can execute.
//  2. Kind cluster deletion — removes the Kind cluster and suite temp dirs.
//
// Cleanup is idempotent: multiple calls are safe (the internal takeKindCluster
// and takeBackend helpers are atomic and nil-out the stored state on first use).
func (t *invocationTeardown) Cleanup(output io.Writer) error {
	var errs []error

	// Step 1: Destroy backend resources (AC5.2) — must happen before the Kind
	// cluster is deleted so that the Docker container is still reachable.
	backendState := t.takeBackend()
	if backendState != nil {
		backendCtx, backendCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer backendCancel()
		if err := backendState.teardown(backendCtx, output); err != nil {
			errs = append(errs, err)
			// Continue to cluster deletion even if backend teardown partially failed.
		}
	}

	// Step 2: Destroy the Kind cluster and suite temp dirs.
	state := t.takeKindCluster()
	if state == nil {
		return errors.Join(errs...)
	}

	ctx, cancel := context.WithTimeout(context.Background(), state.DeleteTimeout)
	defer cancel()

	factory := t.runnerFactory
	if factory == nil {
		factory = func(writer io.Writer) commandRunner {
			return execCommandRunner{Output: writer}
		}
	}

	if err := state.destroyCluster(ctx, factory(output)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (t *invocationTeardown) CleanupWithRunner(ctx context.Context, runner commandRunner) error {
	var errs []error

	// Step 1: Destroy backend resources (ZFS pools, LVM VGs) before the Kind
	// cluster is deleted so that the Docker container is still reachable.
	backendState := t.takeBackend()
	if backendState != nil {
		if err := backendState.teardown(ctx, io.Discard); err != nil {
			errs = append(errs, err)
			// Continue to cluster deletion even if backend teardown partially failed.
		}
	}

	// Step 2: Destroy the Kind cluster and suite temp dirs.
	state := t.takeKindCluster()
	if state == nil {
		return errors.Join(errs...)
	}
	return errors.Join(append(errs, state.destroyCluster(ctx, runner))...)
}

// takeBackend atomically takes and clears the backend state so that Cleanup is
// idempotent: subsequent calls see a nil backend and skip the teardown step.
func (t *invocationTeardown) takeBackend() *suiteBackendState {
	if t == nil {
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	state := t.backend
	t.backend = nil
	return state
}

func (t *invocationTeardown) takeKindCluster() *kindBootstrapState {
	if t == nil {
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	state := t.kind
	t.kind = nil
	return state
}

func sameKindClusterIdentity(left, right *kindBootstrapState) bool {
	if left == nil || right == nil {
		return left == right
	}

	return left.SuiteRootDir == right.SuiteRootDir &&
		left.WorkspaceDir == right.WorkspaceDir &&
		left.LogsDir == right.LogsDir &&
		left.GeneratedDir == right.GeneratedDir &&
		left.ClusterName == right.ClusterName &&
		left.KubeconfigPath == right.KubeconfigPath &&
		left.KindBinary == right.KindBinary &&
		left.KubectlBinary == right.KubectlBinary &&
		left.CreateTimeout == right.CreateTimeout &&
		left.DeleteTimeout == right.DeleteTimeout
}

func installInvocationSignalHandlers(
	teardown *invocationTeardown,
	output io.Writer,
	exit func(int),
) func() {
	if teardown == nil {
		return func() {}
	}

	signals := make(chan os.Signal, 1)
	stop := make(chan struct{})
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	go func() {
		select {
		case sig := <-signals:
			handleInvocationSignal(teardown, output, exit, sig)
		case <-stop:
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			signal.Stop(signals)
			close(stop)
		})
	}
}

func handleInvocationSignal(
	teardown *invocationTeardown,
	output io.Writer,
	exit func(int),
	sig os.Signal,
) {
	if sig == nil {
		return
	}
	if output == nil {
		output = io.Discard
	}

	_, _ = fmt.Fprintf(output, "received %s, cleaning up e2e invocation resources\n", sig)
	if teardown != nil {
		if err := teardown.Cleanup(output); err != nil {
			_, _ = fmt.Fprintf(output, "e2e invocation cleanup failed: %v\n", err)
		}
	}

	if exit != nil {
		exit(exitCodeForSignal(sig))
	}
}

func exitCodeForSignal(sig os.Signal) int {
	signalValue, ok := sig.(syscall.Signal)
	if !ok {
		return 1
	}
	return 128 + int(signalValue)
}
