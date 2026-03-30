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

// Package-internal test helpers that expose Backend fields and constants
// for white-box testing.  This file is compiled only when running `go test`;
// it is NOT part of the production binary.
//
// All helpers operate on a specific *Backend instance rather than on
// package-level globals, so tests that call t.Parallel() are safe to use them
// without data races.
package lvm

import (
	"context"
	"testing"
)

// SetBackendDevBase overrides the devBase field of b for the duration of one
// test.  Because the field is stored per-instance, this is safe to call from
// parallel tests: each test operates on its own Backend and never touches any
// shared global.
//
// The original value is restored automatically via t.Cleanup.
func SetBackendDevBase(t *testing.T, b *Backend, base string) {
	t.Helper()
	orig := b.devBase
	b.devBase = base
	t.Cleanup(func() { b.devBase = orig })
}

// SetBackendExec overrides the executor on a specific Backend instance for the
// duration of one test.  Because the executor is stored per-instance (not as a
// package-level global), this helper is safe to use in parallel tests: each
// test creates its own Backend and injects its own fake executor without
// interfering with any other concurrently-running test.
//
// The original executor is restored automatically via t.Cleanup.
func SetBackendExec(
	t *testing.T,
	b *Backend,
	fn func(ctx context.Context, name string, args ...string) ([]byte, error),
) {
	t.Helper()
	orig := b.exec
	b.exec = execFunc(fn)
	t.Cleanup(func() { b.exec = orig })
}

// CreateThinLV exposes the package-internal createThinLV method for white-box
// unit testing.  It allows tests to exercise the thin LV creation helper
// directly — verifying argument construction, error handling, and idempotency
// logic — without going through the full Create code path.
func CreateThinLV(
	b *Backend,
	ctx context.Context,
	vg, lvName, thinPool string,
	sizeBytes int64,
	extraFlags []string,
) error {
	return b.createThinLV(ctx, vg, lvName, thinPool, sizeBytes, extraFlags)
}

// IsAlreadyExistsOutput exposes isAlreadyExistsOutput for testing so that test
// files can verify the predicate logic without duplicating the string patterns.
func IsAlreadyExistsOutput(out []byte) bool {
	return isAlreadyExistsOutput(out)
}

// SetParamsHasModeOverride sets the unexported hasModeOverride field on a
// Params value for testing.  This allows test code to construct Params with
// an explicit mode override without going through ParseParams (which requires
// a proto message).
func SetParamsHasModeOverride(t *testing.T, p *Params, v bool) {
	t.Helper()
	p.hasModeOverride = v
}
