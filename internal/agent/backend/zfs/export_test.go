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

// Package-internal test helpers that expose ZfsBackend fields and constants
// for white-box testing.  This file is compiled only when running `go test`;
// it is NOT part of the production binary.
//
// All helpers operate on a specific *ZfsBackend instance rather than on
// package-level globals, so tests that call t.Parallel() are safe to use them
// without data races.
package zfs

import (
	"context"
	"testing"
)

// SetBackendDevZvolBase overrides the devZvolBase field of b for the duration
// of one test.  Because the field is stored per-instance, this is safe to
// call from parallel tests: each test operates on its own ZfsBackend and
// never touches any shared global.
//
// The original value is restored automatically via t.Cleanup.
func SetBackendDevZvolBase(t *testing.T, b *ZfsBackend, base string) {
	t.Helper()
	orig := b.devZvolBase
	b.devZvolBase = base
	t.Cleanup(func() { b.devZvolBase = orig })
}

// SetBackendExec overrides the executor on a specific ZfsBackend instance for
// the duration of one test.  Because the executor is stored per-instance (not
// as a package-level global), this helper is safe to use in parallel tests:
// each test creates its own ZfsBackend and injects its own fake executor
// without interfering with any other concurrently-running test.
//
// The original executor is restored automatically via t.Cleanup.
func SetBackendExec(
	t *testing.T,
	b *ZfsBackend,
	fn func(ctx context.Context, name string, args ...string) ([]byte, error),
) {
	t.Helper()
	orig := b.exec
	b.exec = execFunc(fn)
	t.Cleanup(func() { b.exec = orig })
}
