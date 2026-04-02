package e2e

// tc_async_teardown_test.go — Sub-AC 5.3 unit tests.
//
// These tests verify that CloseBackground() returns before cleanup finishes,
// that cleanup errors are propagated through DrainPendingCleanups, and that
// the timeout mechanism correctly abandons a hung cleanup goroutine.

import (
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestCloseBackgroundReturnsBeforeCleanupFinishes verifies the core property of
// Sub-AC 5.3: CloseBackground() must return before the cleanup goroutine
// completes.  We inject a slow cleanup by tracking a custom CleanupFunc that
// sleeps for 200 ms. CloseBackground() must return within 50 ms — well before
// the cleanup goroutine finishes.
//
// Note: this test uses the global suiteAsyncCleanup and must not run in
// parallel with other tests that also use CloseBackground() + DrainPendingCleanups,
// since they share the global batch state.
func TestCloseBackgroundReturnsBeforeCleanupFinishes(t *testing.T) {
	// Not parallel: uses the global suiteAsyncCleanup singleton.

	scope, err := NewTestCaseScope("async-teardown-timing-test")
	if err != nil {
		t.Fatalf("NewTestCaseScope: %v", err)
	}

	// Track a resource whose cleanup sleeps 200 ms.
	const slowCleanupDelay = 200 * time.Millisecond
	cleanupDone := make(chan struct{})
	var cleanupStarted atomic.Bool

	if err := scope.TrackVolume("slow-vol", PathResourceSpec{
		Path: scope.Path("vol", "data"),
		Cleanup: func() error {
			cleanupStarted.Store(true)
			time.Sleep(slowCleanupDelay)
			close(cleanupDone)
			return nil
		},
		IsPresent: func() (bool, error) { return false, nil },
	}); err != nil {
		t.Fatalf("TrackVolume: %v", err)
	}

	// Measure how long CloseBackground takes to return.
	const returnDeadline = 50 * time.Millisecond
	start := time.Now()
	scope.CloseBackground()
	elapsed := time.Since(start)

	if elapsed >= returnDeadline {
		t.Errorf("CloseBackground() took %v to return; want < %v (must return before cleanup goroutine finishes)",
			elapsed, returnDeadline)
	}

	// Wait for the cleanup goroutine to finish via a local channel (avoids
	// interference with other tests sharing the global suiteAsyncCleanup).
	select {
	case <-cleanupDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup goroutine did not finish within 5 seconds")
	}

	if !cleanupStarted.Load() {
		t.Error("cleanup goroutine did not start during the drain window")
	}

	// Also drain the global batch to leave a clean state for subsequent tests.
	if err := DrainPendingCleanups(5 * time.Second); err != nil {
		t.Logf("DrainPendingCleanups: %v (informational)", err)
	}
}

// TestCloseBackgroundCleanupResultDeliveredToDrain verifies that the cleanup
// result (nil on success) is collected by DrainPendingCleanups.
//
// Note: not parallel — uses global suiteAsyncCleanup.
func TestCloseBackgroundCleanupResultDeliveredToDrain(t *testing.T) {
	// Not parallel: uses the global suiteAsyncCleanup singleton.

	scope, err := NewTestCaseScope("async-teardown-success-result")
	if err != nil {
		t.Fatalf("NewTestCaseScope: %v", err)
	}

	scope.CloseBackground()

	if err := DrainPendingCleanups(5 * time.Second); err != nil {
		t.Errorf("DrainPendingCleanups after successful CloseBackground: %v", err)
	}
}

// TestCloseBackgroundCleanupErrorPropagatedViaDrain verifies that when cleanup
// fails (a tracked resource remains present after teardown), the error is
// propagated through DrainPendingCleanups — not lost in the background.
//
// Note: not parallel — uses global suiteAsyncCleanup.
func TestCloseBackgroundCleanupErrorPropagatedViaDrain(t *testing.T) {
	// Not parallel: uses the global suiteAsyncCleanup singleton.

	scope, err := NewTestCaseScope("async-teardown-error-prop")
	if err != nil {
		t.Fatalf("NewTestCaseScope: %v", err)
	}

	// Track a resource whose cleanup is a no-op (resource "remains" present).
	if err := scope.TrackVolume("leaked-vol", PathResourceSpec{
		Path:    scope.Path("vol", "leaked"),
		Cleanup: func() error { return nil }, // no-op: resource stays
		IsPresent: func() (bool, error) {
			return true, nil // always present — simulates a leak
		},
	}); err != nil {
		t.Fatalf("TrackVolume: %v", err)
	}

	scope.CloseBackground()

	drainErr := DrainPendingCleanups(5 * time.Second)
	if drainErr == nil {
		t.Fatal("DrainPendingCleanups: expected error (resource leaked), got nil")
	}
	t.Logf("DrainPendingCleanups correctly reported cleanup error: %v", drainErr)
}

// TestDrainPendingCleanupsIsIdempotent verifies that calling DrainPendingCleanups
// twice is safe: the second call returns nil immediately without blocking.
func TestDrainPendingCleanupsIsIdempotent(t *testing.T) {
	t.Parallel()

	batch := newPendingCleanupBatch()

	// Register and resolve one cleanup.
	ch := make(chan backgroundCleanupResult, 1)
	batch.track(ch)
	ch <- backgroundCleanupResult{tcID: "idempotent-tc", err: nil}

	// First drain collects the result.
	if err := batch.drain(time.Second); err != nil {
		t.Fatalf("first drain: %v", err)
	}

	// Second drain must return nil immediately (nothing left to drain).
	start := time.Now()
	if err := batch.drain(time.Second); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("second drain took %v; want near-instant return on empty batch", elapsed)
	}
}

// TestDrainPendingCleanupsTimesOutOnHungGoroutine verifies that drain() returns
// after the specified timeout even if a goroutine never sends its result. This
// prevents a hung cleanup from stalling the suite teardown indefinitely.
func TestDrainPendingCleanupsTimesOutOnHungGoroutine(t *testing.T) {
	t.Parallel()

	batch := newPendingCleanupBatch()

	// Register a channel that will never receive a result (simulates a hung goroutine).
	hung := make(chan backgroundCleanupResult) // unbuffered, never written
	batch.track(hung)

	const drainTimeout = 100 * time.Millisecond
	start := time.Now()
	err := batch.drain(drainTimeout)
	elapsed := time.Since(start)

	// Must return approximately at the timeout (not block forever).
	const toleranceMs = 150
	if elapsed > drainTimeout+toleranceMs*time.Millisecond {
		t.Errorf("drain hung for %v; want return within ~%v",
			elapsed, drainTimeout+toleranceMs*time.Millisecond)
	}

	// Must report a timeout error.
	if err == nil {
		t.Fatal("drain returned nil; want timeout error for hung goroutine")
	}
	t.Logf("drain correctly timed out after %v: %v", elapsed, err)
}

// TestDrainPendingCleanupsAggregatesMultipleErrors verifies that when multiple
// cleanup goroutines fail, all errors are reported (not just the first).
func TestDrainPendingCleanupsAggregatesMultipleErrors(t *testing.T) {
	t.Parallel()

	batch := newPendingCleanupBatch()

	// Register three cleanups: one success and two failures.
	for _, tc := range []struct {
		tcID string
		err  error
	}{
		{"tc-ok", nil},
		{"tc-fail-1", errors.New("first cleanup failure")},
		{"tc-fail-2", errors.New("second cleanup failure")},
	} {
		ch := make(chan backgroundCleanupResult, 1)
		batch.track(ch)
		ch <- backgroundCleanupResult{tcID: tc.tcID, err: tc.err}
	}

	err := batch.drain(time.Second)
	if err == nil {
		t.Fatal("drain returned nil; want aggregated errors from two failed cleanups")
	}

	// errors.Join produces a message containing both sub-errors.
	msg := err.Error()
	if !containsString(msg, "first cleanup failure") {
		t.Errorf("aggregated error missing 'first cleanup failure': %v", err)
	}
	if !containsString(msg, "second cleanup failure") {
		t.Errorf("aggregated error missing 'second cleanup failure': %v", err)
	}
	t.Logf("drain aggregated errors: %v", err)
}

// TestCloseBackgroundNextTCStartsBeforeCleanupFinishes verifies the core
// performance property of Sub-AC 5.3: the *next* TC is able to start its setup
// before the *previous* TC's cleanup goroutine finishes.
//
// This is a timing test that confirms no blocking occurs between the cleanup
// goroutine spawning and the next setup phase beginning. It uses a local done
// channel rather than DrainPendingCleanups to avoid interference with the
// global suiteAsyncCleanup batch in parallel test runs.
//
// Note: not parallel — uses global suiteAsyncCleanup.
func TestCloseBackgroundNextTCStartsBeforeCleanupFinishes(t *testing.T) {
	// Not parallel: uses the global suiteAsyncCleanup singleton.

	const slowCleanupDelay = 200 * time.Millisecond

	// TC1: scope with a slow cleanup.
	scope1, err := NewTestCaseScope("async-concurrency-tc1")
	if err != nil {
		t.Fatalf("TC1 NewTestCaseScope: %v", err)
	}

	var cleanupFinished atomic.Bool
	cleanupDone := make(chan struct{})
	if err := scope1.TrackVolume("slow-cleanup-vol", PathResourceSpec{
		Path: scope1.Path("vol", "data"),
		Cleanup: func() error {
			time.Sleep(slowCleanupDelay)
			cleanupFinished.Store(true)
			close(cleanupDone)
			return nil
		},
		IsPresent: func() (bool, error) { return false, nil },
	}); err != nil {
		t.Fatalf("TrackVolume: %v", err)
	}

	// Fire TC1 cleanup in the background (simulates DeferCleanup calling CloseBackground).
	scope1.CloseBackground()

	// TC2 starts immediately without waiting for TC1's cleanup.
	scope2, err := NewTestCaseScope("async-concurrency-tc2")
	if err != nil {
		t.Fatalf("TC2 NewTestCaseScope: %v", err)
	}
	defer func() {
		if err := scope2.Close(); err != nil {
			t.Logf("scope2.Close: %v", err)
		}
	}()

	// At this point TC1's cleanup goroutine should still be running (it sleeps 200ms).
	if cleanupFinished.Load() {
		t.Error("TC1 cleanup already finished — expected it to still be running when TC2 starts (should be concurrent)")
	}

	// Wait for TC1 cleanup completion via a local done channel. This avoids
	// global suiteAsyncCleanup interference while still verifying cleanup
	// eventually completes.
	select {
	case <-cleanupDone:
	case <-time.After(5 * time.Second):
		t.Fatal("TC1 cleanup goroutine did not finish within 5 seconds")
	}

	if !cleanupFinished.Load() {
		t.Error("TC1 cleanup finished channel closed but atomic flag not set (race?)")
	}

	// Drain the global batch so we don't leave a pending channel for subsequent tests.
	if err := DrainPendingCleanups(5 * time.Second); err != nil {
		t.Logf("DrainPendingCleanups: %v (informational)", err)
	}
}

// TestCloseBackgroundScopeAlreadyClosedIsHarmless verifies that calling
// CloseBackground() on a scope that has already been closed (e.g. via a direct
// Close() call) does not panic or block. The background goroutine runs Close()
// again, which returns nil due to the closed guard in TestCaseScope.Close().
//
// Note: not parallel — uses global suiteAsyncCleanup.
func TestCloseBackgroundScopeAlreadyClosedIsHarmless(t *testing.T) {
	// Not parallel: uses the global suiteAsyncCleanup singleton.

	scope, err := NewTestCaseScope("async-teardown-double-close")
	if err != nil {
		t.Fatalf("NewTestCaseScope: %v", err)
	}

	// Synchronous close first.
	if err := scope.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// Background close on an already-closed scope must not panic.
	scope.CloseBackground()

	if err := DrainPendingCleanups(5 * time.Second); err != nil {
		t.Errorf("DrainPendingCleanups after double-close: %v", err)
	}
}

// TestDrainPendingCleanupsOnEmptyBatchReturnsNilImmediately verifies that
// calling DrainPendingCleanups when no background cleanups are pending returns
// nil near-instantly (no blocking).
func TestDrainPendingCleanupsOnEmptyBatchReturnsNilImmediately(t *testing.T) {
	t.Parallel()

	batch := newPendingCleanupBatch()

	start := time.Now()
	if err := batch.drain(30 * time.Second); err != nil {
		t.Fatalf("drain on empty batch: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("drain on empty batch took %v; want near-instant return", elapsed)
	}
}

// TestCloseBackgroundRootDirRemovedByDrainTime verifies the practical outcome:
// after CloseBackground() and DrainPendingCleanups(), the TC's RootDir has
// been removed from the filesystem (normal cleanup path).
//
// Note: not parallel — uses global suiteAsyncCleanup.
func TestCloseBackgroundRootDirRemovedByDrainTime(t *testing.T) {
	// Not parallel: uses the global suiteAsyncCleanup singleton.

	scope, err := NewTestCaseScope("async-teardown-rootdir-removal")
	if err != nil {
		t.Fatalf("NewTestCaseScope: %v", err)
	}

	rootDir := scope.RootDir
	if _, err := os.Stat(rootDir); err != nil {
		t.Fatalf("RootDir %s does not exist before CloseBackground: %v", rootDir, err)
	}

	scope.CloseBackground()

	if err := DrainPendingCleanups(5 * time.Second); err != nil {
		t.Fatalf("DrainPendingCleanups: %v", err)
	}

	if _, err := os.Stat(rootDir); !os.IsNotExist(err) {
		t.Errorf("RootDir %s still exists after CloseBackground+DrainPendingCleanups", rootDir)
	}
}

// TestPendingCleanupBatchTrackAndDrainMultipleChannels verifies that the
// pendingCleanupBatch correctly tracks and drains multiple channels in a single
// drain call, processing all results regardless of completion order.
func TestPendingCleanupBatchTrackAndDrainMultipleChannels(t *testing.T) {
	t.Parallel()

	const numChannels = 10
	batch := newPendingCleanupBatch()

	// Register 10 channels, all pre-filled with success results.
	for i := 0; i < numChannels; i++ {
		ch := make(chan backgroundCleanupResult, 1)
		batch.track(ch)
		ch <- backgroundCleanupResult{tcID: "batch-tc", err: nil}
	}

	if err := batch.drain(time.Second); err != nil {
		t.Fatalf("drain 10 successful cleanups: %v", err)
	}

	// Second drain must be empty (all channels consumed).
	if err := batch.drain(time.Second); err != nil {
		t.Fatalf("second drain after consuming all channels: %v", err)
	}
}

// containsString is a helper that avoids importing strings in test-only code.
func containsString(haystack, needle string) bool {
	return len(haystack) >= len(needle) && searchString(haystack, needle)
}

func searchString(haystack, needle string) bool {
	for i := 0; i <= len(haystack)-len(needle); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
