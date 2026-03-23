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

// export_test.go exposes internal package variables for white-box testing.
// This file is compiled only when running `go test`; it is NOT part of the
// production binary.

package zfs

import "testing"

// SetDevZvolBase overrides devZvolBase for the duration of one test.
// The original value is restored automatically via t.Cleanup.
func SetDevZvolBase(t *testing.T, base string) {
	t.Helper()
	orig := devZvolBase
	devZvolBase = base
	t.Cleanup(func() { devZvolBase = orig })
}
