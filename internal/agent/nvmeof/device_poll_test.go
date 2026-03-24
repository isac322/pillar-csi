/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// White-box tests: same package gives direct access to WaitForDevice.
package nvmeof

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWaitForDevice_AlreadyPresent verifies that WaitForDevice returns nil
// immediately when the path already exists at the first probe.
func TestWaitForDevice_AlreadyPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev-exists")

	// Create the file before calling WaitForDevice.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create file: %v", err)
	}

	start := time.Now()
	err := WaitForDevice(context.Background(), path, 50*time.Millisecond, 2*time.Second, nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	// Should return well under one poll interval (no sleep needed).
	if elapsed > 200*time.Millisecond {
		t.Errorf("expected fast return for pre-existing device, took %s", elapsed)
	}
}

// TestWaitForDevice_AppearsAfterDelay verifies that WaitForDevice succeeds
// when the path is created asynchronously after a short delay — modeling
// udev settling after a zfs(8) create command.
func TestWaitForDevice_AppearsAfterDelay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev-delayed")

	// Create the file after 120 ms in a background goroutine.
	const createDelay = 120 * time.Millisecond
	go func() {
		time.Sleep(createDelay)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			// Nothing useful to do inside a goroutine; the test will fail via timeout.
			return
		}
	}()

	err := WaitForDevice(context.Background(), path, 50*time.Millisecond, 2*time.Second, nil)
	if err != nil {
		t.Fatalf("expected nil error after device appeared, got %v", err)
	}
}

// TestWaitForDevice_Timeout verifies that WaitForDevice returns a non-nil
// error when the path never appears within the given timeout.
func TestWaitForDevice_Timeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev-never")
	// Deliberately do NOT create the file.

	err := WaitForDevice(context.Background(), path, 20*time.Millisecond, 100*time.Millisecond, nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// The error message must mention the path so callers can diagnose failures.
	if !containsSubstring(err.Error(), path) {
		t.Errorf("error %q does not mention path %q", err.Error(), path)
	}
}

// TestWaitForDevice_ContextCanceled verifies that WaitForDevice respects
// context cancellation and returns promptly when the context is canceled.
func TestWaitForDevice_ContextCanceled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev-cancel")
	// Deliberately do NOT create the file.

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel the context after a short delay.
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := WaitForDevice(ctx, path, 20*time.Millisecond, 10*time.Second, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
	// Should return well before the 10 s timeout.
	if elapsed > 500*time.Millisecond {
		t.Errorf("expected early return on context cancel, took %s", elapsed)
	}
	// The returned error must wrap context.Canceled so callers can identify it.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected error to wrap context.Canceled, got: %v", err)
	}
}

// TestWaitForDevice_StatError verifies that WaitForDevice returns immediately
// (without further retries) when os.Stat returns an error that is not
// "not-exist" — e.g. a permission-denied error.
//
// We simulate this by creating a file at the path and then removing read
// permissions from its parent directory so Stat returns EACCES.
func TestWaitForDevice_StatError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("cannot test permission error when running as root")
	}

	dir := t.TempDir()
	subdir := filepath.Join(dir, "noperm")
	if err := os.Mkdir(subdir, 0o000); err != nil {
		t.Fatalf("mkdir noperm: %v", err)
	}
	t.Cleanup(func() {
		// Restore permissions so TempDir cleanup can remove the directory.
		//nolint:gosec // G302: test cleanup; restoring traversal bits for os.RemoveAll.
		bestEffort(os.Chmod(subdir, 0o755))
	})

	path := filepath.Join(subdir, "device")

	err := WaitForDevice(context.Background(), path, 20*time.Millisecond, 2*time.Second, nil)
	if err == nil {
		t.Fatal("expected error for permission-denied path, got nil")
	}
	// Must NOT be a timeout error — should return immediately.
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected immediate error, got timeout: %v", err)
	}
}

// TestWaitForDevice_DefaultConstants verifies that the exported default
// constants have sensible values (non-zero, timeout > interval).
func TestWaitForDevice_DefaultConstants(t *testing.T) {
	if DefaultDevicePollInterval <= 0 {
		t.Errorf("DefaultDevicePollInterval must be positive, got %s", DefaultDevicePollInterval)
	}
	if DefaultDevicePollTimeout <= 0 {
		t.Errorf("DefaultDevicePollTimeout must be positive, got %s", DefaultDevicePollTimeout)
	}
	if DefaultDevicePollTimeout <= DefaultDevicePollInterval {
		t.Errorf("DefaultDevicePollTimeout (%s) must be > DefaultDevicePollInterval (%s)",
			DefaultDevicePollTimeout, DefaultDevicePollInterval)
	}
}

// TestWaitForDevice_MockChecker_SuccessOnFirstPoll verifies that WaitForDevice
// returns nil immediately when the injected mock checker reports the device
// present on the very first probe — no sleep cycle should occur.
func TestWaitForDevice_MockChecker_SuccessOnFirstPoll(t *testing.T) {
	calls := 0
	mock := DeviceChecker(func(_ string) (bool, error) {
		calls++
		return true, nil // device present on the very first call
	})

	start := time.Now()
	err := WaitForDevice(context.Background(), "/fake/dev/present", 50*time.Millisecond, 2*time.Second, mock)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 checker call, got %d", calls)
	}
	// No sleep is needed when the device is present immediately.
	if elapsed > 200*time.Millisecond {
		t.Errorf("expected fast return for present device, took %s", elapsed)
	}
}

// TestWaitForDevice_MockChecker_SuccessOnNthPoll verifies that WaitForDevice
// succeeds when the mock checker returns absent for the first N-1 polls and
// present on the Nth poll, and that it makes exactly N checker calls.
func TestWaitForDevice_MockChecker_SuccessOnNthPoll(t *testing.T) {
	const presentOnCall = 3
	calls := 0
	mock := DeviceChecker(func(_ string) (bool, error) {
		calls++
		if calls >= presentOnCall {
			return true, nil // present starting on the Nth call
		}
		return false, nil // absent on earlier calls
	})

	err := WaitForDevice(context.Background(), "/fake/dev/delayed", 20*time.Millisecond, 2*time.Second, mock)
	if err != nil {
		t.Fatalf("expected nil error after device appeared on call %d, got %v", presentOnCall, err)
	}
	if calls != presentOnCall {
		t.Errorf("expected exactly %d checker calls, got %d", presentOnCall, calls)
	}
}

// TestWaitForDevice_MockChecker_Timeout verifies that WaitForDevice returns a
// non-nil error wrapping context.DeadlineExceeded when the mock checker never
// reports the device as present and the timeout is exhausted.
func TestWaitForDevice_MockChecker_Timeout(t *testing.T) {
	calls := 0
	mock := DeviceChecker(func(_ string) (bool, error) {
		calls++
		return false, nil // device never present
	})

	const path = "/fake/dev/never"
	err := WaitForDevice(context.Background(), path, 20*time.Millisecond, 80*time.Millisecond, mock)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// The returned error must wrap DeadlineExceeded so callers can detect
	// timeout programmatically via errors.Is.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected error to wrap context.DeadlineExceeded, got: %v", err)
	}
	// The error message must mention the path so operators can diagnose failures.
	if !containsSubstring(err.Error(), path) {
		t.Errorf("error %q does not mention path %q", err.Error(), path)
	}
	// The checker must have been called at least once before giving up.
	if calls == 0 {
		t.Error("expected at least one checker call before timeout")
	}
}

// TestWaitForDevice_MockChecker_PermanentError verifies that WaitForDevice
// returns immediately (no retries) when the mock checker signals a permanent
// error, and that the returned error wraps the original sentinel.
func TestWaitForDevice_MockChecker_PermanentError(t *testing.T) {
	sentinel := errors.New("permanent: permission denied on /fake/dev/perm")
	calls := 0
	mock := DeviceChecker(func(_ string) (bool, error) {
		calls++
		return false, sentinel // permanent error on every call
	})

	start := time.Now()
	err := WaitForDevice(context.Background(), "/fake/dev/perm", 20*time.Millisecond, 5*time.Second, mock)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected permanent error, got nil")
	}
	// Permanent error must be propagated (WaitForDevice returns it directly).
	if !errors.Is(err, sentinel) {
		t.Errorf("expected error to wrap sentinel, got: %v", err)
	}
	// Must stop after exactly one call — no retries for permanent errors.
	if calls != 1 {
		t.Errorf("expected exactly 1 checker call for permanent error, got %d", calls)
	}
	// Must return quickly, not after sleeping for the full timeout.
	if elapsed > 500*time.Millisecond {
		t.Errorf("expected immediate return on permanent error, took %s", elapsed)
	}
}

// containsSubstring is a simple helper to avoid importing strings package.
func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && func() bool {
		for i := range len(s) - len(sub) + 1 {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}
