package e2e

// tc_async_teardown.go — Sub-AC 5.3: fast ephemeral resource cleanup via
// background goroutine deletion with timeout.
//
// # Problem
//
// The synchronous Close() path blocks the DeferCleanup hook until all tracked
// resources (mounts, volumes, processes) are fully cleaned up and verified
// absent.  For tests that track slow-to-clean resources (e.g. processes that
// need SIGTERM → wait → SIGKILL → wait), this teardown adds latency to every
// inter-TC gap, preventing the next test from starting.
//
// # Solution
//
// CloseBackground() fires the existing synchronous Close() inside a background
// goroutine and returns immediately.  The calling DeferCleanup hook exits
// without waiting, so the next TC's BeforeEach phase begins at once.
//
// The background goroutine is bounded by backgroundCleanupTimeout (30 s) so
// that a hang in cleanup cannot stall the suite indefinitely.  On timeout the
// error is recorded and the goroutine is abandoned (the inner Close goroutine
// continues running and will eventually free resources, but the suite no longer
// waits for it).
//
// # Suite drain
//
// Each in-flight background cleanup registers a result channel with
// suiteAsyncCleanup (the process-local pendingCleanupBatch).  AfterSuite calls
// DrainPendingCleanups(timeout) to collect all results before the Kind cluster
// is deleted.  Cleanup errors are logged to GinkgoWriter but do not fail the
// suite — by the time AfterSuite runs, all spec pass/fail decisions are final.
//
// # Lifecycle invariants
//
//	CloseBackground() → goroutine running Close() → result channel ← DrainPendingCleanups
//	                         ↑                                              ↑
//	             registered in suiteAsyncCleanup              drains suiteAsyncCleanup

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// backgroundCleanupTimeout is the maximum duration allowed for a single
// background TC cleanup goroutine. After this deadline the cleanup result is
// marked as a timeout error and the goroutine is abandoned (it continues
// running but the suite no longer waits for it).
//
// 30 seconds is generous: filesystem removal and process kill are typically
// sub-second. The budget absorbs edge cases such as a slow unmount or a process
// that ignores SIGTERM and must wait for SIGKILL.
const backgroundCleanupTimeout = 30 * time.Second

// backgroundCleanupResult captures the outcome of one async TC cleanup
// goroutine. It is sent exactly once on the channel registered with
// suiteAsyncCleanup.
type backgroundCleanupResult struct {
	tcID string
	err  error
}

// pendingCleanupBatch accumulates the result channels of in-flight background
// TC cleanup goroutines. DrainPendingCleanups reads from every registered
// channel so the suite can wait for all cleanups before exiting.
//
// Each Ginkgo worker process has its own instance (package-level state), which
// is correct because each worker only drains its own TCs.
type pendingCleanupBatch struct {
	mu      sync.Mutex
	results []<-chan backgroundCleanupResult
}

// suiteAsyncCleanup is the process-local registry of in-flight background TC
// cleanup goroutines. CloseBackground() registers channels here; AfterSuite
// drains it via DrainPendingCleanups.
var suiteAsyncCleanup = newPendingCleanupBatch()

func newPendingCleanupBatch() *pendingCleanupBatch {
	return &pendingCleanupBatch{}
}

// track registers ch so DrainPendingCleanups can wait for its result.  ch must
// have a buffer capacity of at least 1 and must be written exactly once by the
// background goroutine.
func (b *pendingCleanupBatch) track(ch <-chan backgroundCleanupResult) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.results = append(b.results, ch)
}

// drain waits for all tracked background cleanups to finish, up to timeout.
// It consumes and removes all registered channels on each call, making it safe
// to call repeatedly (a second call after a successful drain returns nil with
// no work to do).
//
// If the overall timeout elapses before all goroutines report, remaining
// goroutines are considered timed out and an error is recorded for each.
// Goroutines that time out are abandoned — they continue running in the
// background but the suite no longer waits for them.
func (b *pendingCleanupBatch) drain(timeout time.Duration) error {
	b.mu.Lock()
	channels := b.results
	b.results = nil
	b.mu.Unlock()

	if len(channels) == 0 {
		return nil
	}

	deadline := time.Now().Add(timeout)
	var errs []error

	for _, ch := range channels {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			errs = append(errs, fmt.Errorf("background cleanup drain exceeded %v total timeout", timeout))
			continue
		}

		select {
		case r := <-ch:
			if r.err != nil {
				errs = append(errs, fmt.Errorf("background cleanup for TC %q: %w", r.tcID, r.err))
			}
		case <-time.After(remaining):
			errs = append(errs, fmt.Errorf("background cleanup drain exceeded %v total timeout", timeout))
		}
	}

	return errors.Join(errs...)
}

// DrainPendingCleanups waits for all in-flight background TC cleanup goroutines
// to finish, up to the given overall timeout. It should be called from
// AfterSuite (before cluster deletion) so that no cleanup goroutines are still
// running when the process exits.
//
// A non-nil return value means that one or more cleanup goroutines either
// failed or timed out. These errors are informational: spec pass/fail decisions
// are already final when AfterSuite runs, so callers should log rather than
// fail on a non-nil result.
//
// Calling DrainPendingCleanups is idempotent: a second call after all channels
// have been consumed returns nil immediately.
func DrainPendingCleanups(timeout time.Duration) error {
	return suiteAsyncCleanup.drain(timeout)
}

// CloseBackground starts TC scope cleanup in a background goroutine and
// returns immediately, allowing the next test case to begin before this TC's
// cleanup finishes.
//
// Behaviour:
//
//  1. Spawns a goroutine that calls the synchronous s.Close().
//  2. Registers a buffered result channel with suiteAsyncCleanup so
//     DrainPendingCleanups can collect the result at suite teardown time.
//  3. Wraps the Close() goroutine with a backgroundCleanupTimeout deadline.
//     If Close() exceeds the deadline, a timeout error is sent on the channel
//     and the inner goroutine is abandoned (it continues running but the suite
//     no longer waits for it).
//
// This method is the preferred teardown path inside UsePerTestCaseSetup's
// DeferCleanup. Tests that need synchronous cleanup (e.g. tests that inspect
// the absence of resources in subsequent It blocks within an Ordered container)
// should call DrainPendingCleanups before checking for resource absence, or
// use the synchronous Close() instead.
func (s *TestCaseScope) CloseBackground() {
	tcID := s.TCID

	// Buffered channel (capacity 1): the goroutine writes exactly once and
	// DrainPendingCleanups reads exactly once. The buffer prevents the goroutine
	// from blocking if DrainPendingCleanups is called before the goroutine exits.
	ch := make(chan backgroundCleanupResult, 1)
	suiteAsyncCleanup.track(ch)

	go func() {
		timer := time.NewTimer(backgroundCleanupTimeout)
		defer timer.Stop()

		// Inner goroutine runs the synchronous Close(). We can't cancel it, but
		// we can stop waiting for it when the timeout fires. If the inner goroutine
		// eventually completes it writes to done (buffered, so it never blocks).
		type closeResult struct{ err error }
		done := make(chan closeResult, 1)
		go func() {
			done <- closeResult{err: s.Close()}
		}()

		select {
		case r := <-done:
			ch <- backgroundCleanupResult{tcID: tcID, err: r.err}
		case <-timer.C:
			ch <- backgroundCleanupResult{
				tcID: tcID,
				err: fmt.Errorf(
					"TC %q cleanup goroutine exceeded %v timeout (abandoned; resources may not be fully released)",
					tcID, backgroundCleanupTimeout,
				),
			}
		}
	}()
}
