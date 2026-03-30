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

package lvm_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/agent/backend/lvm"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

// call records one invocation of the fake executor.
type call struct {
	name string
	args []string
}

// fakeExec is a sequential test double for the LVM executor.  Each entry in
// responses is consumed in order; if more calls are made than responses
// provided the test fails immediately.
type fakeExec struct {
	t         *testing.T
	mu        sync.Mutex
	responses []fakeResponse
	pos       int
	calls     []call
}

type fakeResponse struct {
	out []byte
	err error
}

func newFake(t *testing.T, responses ...fakeResponse) *fakeExec {
	t.Helper()
	return &fakeExec{t: t, responses: responses}
}

func (f *fakeExec) exec() func(_ context.Context, name string, args ...string) ([]byte, error) {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()

		f.calls = append(f.calls, call{name: name, args: args})
		if f.pos >= len(f.responses) {
			f.t.Fatalf("fakeExec: unexpected call #%d: %s %v", f.pos+1, name, args)
		}
		resp := f.responses[f.pos]
		f.pos++
		return resp.out, resp.err
	}
}

// assertCallCount fails the test if the fake was not called exactly n times.
func (f *fakeExec) assertCallCount(n int) {
	f.t.Helper()
	if f.pos != n {
		f.t.Errorf("fakeExec: expected %d call(s), got %d", n, f.pos)
	}
}

// assertArgsContain fails the test if the i-th call does not contain all of
// the expected strings anywhere in its argument list.
func (f *fakeExec) assertArgsContain(callIdx int, fragments ...string) {
	f.t.Helper()
	if callIdx >= len(f.calls) {
		f.t.Errorf("fakeExec: no call at index %d", callIdx)
		return
	}
	c := f.calls[callIdx]
	allArgs := append([]string{c.name}, c.args...)
	joined := strings.Join(allArgs, " ")
	for _, frag := range fragments {
		if !strings.Contains(joined, frag) {
			f.t.Errorf("fakeExec call[%d] %q: expected to contain %q", callIdx, joined, frag)
		}
	}
}

// CallName returns the command name of the i-th call.
func (f *fakeExec) CallName(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.calls) {
		f.t.Fatalf("fakeExec: no call at index %d", i)
	}
	return f.calls[i].name
}

// CallArgs returns the arguments of the i-th call (excluding the command name).
func (f *fakeExec) CallArgs(i int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.calls) {
		f.t.Fatalf("fakeExec: no call at index %d", i)
	}
	return f.calls[i].args
}

// ok is a helper for a successful fake response with the given output.
func ok(out string) fakeResponse { return fakeResponse{out: []byte(out)} }

// fail is a helper for a failed fake response with the given stderr output.
func fail(out string) fakeResponse {
	return fakeResponse{out: []byte(out), err: errors.New("exit status 5")}
}

// lvNotExistResp simulates the LVM "Failed to find logical volume" error output.
func lvNotExistResp(vg, lv string) fakeResponse {
	return fail(fmt.Sprintf("  Failed to find logical volume \"%s/%s\"", vg, lv))
}

// ─────────────────────────────────────────────────────────────────────────────
// DevicePath tests (no executor needed)
// ─────────────────────────────────────────────────────────────────────────────

func TestDevicePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		vg       string
		thinpool string
		volumeID string
		want     string
	}{
		{
			name:     "linear lv",
			vg:       "data-vg",
			thinpool: "",
			volumeID: "data-vg/pvc-abc123",
			want:     "/dev/data-vg/pvc-abc123",
		},
		{
			name:     "thin lv",
			vg:       "data-vg",
			thinpool: "thin-pool-0",
			volumeID: "data-vg/pvc-xyz",
			want:     "/dev/data-vg/pvc-xyz",
		},
		{
			name:     "single-letter vg",
			vg:       "v",
			thinpool: "",
			volumeID: "v/vol0",
			want:     "/dev/v/vol0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := lvm.New(tc.vg, tc.thinpool)
			got := b.DevicePath(tc.volumeID)
			if got != tc.want {
				t.Errorf("DevicePath(%q) = %q; want %q", tc.volumeID, got, tc.want)
			}
		})
	}
}

func TestDevicePathCustomBase(t *testing.T) {
	t.Parallel()

	b := lvm.New("data-vg", "")
	lvm.SetBackendDevBase(t, b, "/tmp/fakedev")

	got := b.DevicePath("data-vg/pvc-1")
	want := "/tmp/fakedev/data-vg/pvc-1"
	if got != want {
		t.Errorf("DevicePath with custom base = %q; want %q", got, want)
	}
}

// TestDevicePath_ThinSameAsLinear verifies that DevicePath returns the same
// kernel device path format for thin-provisioned LVs as for linear LVs.
// The kernel exposes both at /dev/<vg>/<lv-name> regardless of provisioning
// mode; the distinction exists only at the LVM metadata level.
func TestDevicePath_ThinSameAsLinear(t *testing.T) {
	t.Parallel()

	linearB := lvm.New("data-vg", "")
	thinB := lvm.New("data-vg", "thin-pool-0")

	volumeID := "data-vg/pvc-shared"

	linearPath := linearB.DevicePath(volumeID)
	thinPath := thinB.DevicePath(volumeID)

	if linearPath != thinPath {
		t.Errorf("DevicePath differs between linear (%q) and thin (%q) for same volumeID",
			linearPath, thinPath)
	}
	want := "/dev/data-vg/pvc-shared"
	if linearPath != want {
		t.Errorf("DevicePath = %q; want %q", linearPath, want)
	}
}

// TestDevicePath_NoVGPrefix verifies that DevicePath handles a volumeID that
// does not include the VG prefix (bare LV name).  The lvName helper strips
// only the "<vg>/" prefix via strings.TrimPrefix, so a bare name is preserved
// as-is and DevicePath still returns a valid /dev/<vg>/<lv> path.
func TestDevicePath_NoVGPrefix(t *testing.T) {
	t.Parallel()

	b := lvm.New("data-vg", "")
	// VolumeID without the "data-vg/" prefix — still must return a coherent path.
	got := b.DevicePath("pvc-bare")
	want := "/dev/data-vg/pvc-bare"
	if got != want {
		t.Errorf("DevicePath (no prefix) = %q; want %q", got, want)
	}
}

// TestDevicePath_AllComponents verifies that all three path components
// (devBase, VG name, LV name) appear in the returned path in the correct order.
// This is a structural sanity-check that no component is accidentally dropped
// or duplicated.
func TestDevicePath_AllComponents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		vg       string
		thinpool string
		volumeID string
		want     string
	}{
		{
			name:     "hyphenated vg and lv",
			vg:       "my-data-vg",
			thinpool: "",
			volumeID: "my-data-vg/pvc-abc-123",
			want:     "/dev/my-data-vg/pvc-abc-123",
		},
		{
			name:     "numeric suffix vg",
			vg:       "vg0",
			thinpool: "",
			volumeID: "vg0/vol0",
			want:     "/dev/vg0/vol0",
		},
		{
			name:     "thin lv different pool name",
			vg:       "nvme-vg",
			thinpool: "nvme-pool",
			volumeID: "nvme-vg/pvc-fast",
			want:     "/dev/nvme-vg/pvc-fast",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := lvm.New(tc.vg, tc.thinpool)
			got := b.DevicePath(tc.volumeID)
			if got != tc.want {
				t.Errorf("DevicePath(%q) = %q; want %q", tc.volumeID, got, tc.want)
			}
		})
	}
}

// TestDevicePath_MakesNoCLICalls verifies that DevicePath is a pure
// metadata-only operation that constructs the device path from the VG and LV
// name without executing any CLI commands.  An executor that panics on any
// call is injected to enforce this guarantee.
func TestDevicePath_MakesNoCLICalls(t *testing.T) {
	t.Parallel()

	b := lvm.New("data-vg", "")
	// Inject an executor that fails the test immediately if called.
	lvm.SetBackendExec(t, b, func(_ context.Context, name string, args ...string) ([]byte, error) {
		t.Fatalf("DevicePath unexpectedly invoked CLI: %s %v", name, args)
		return nil, nil
	})

	// Must not trigger the executor above.
	got := b.DevicePath("data-vg/pvc-no-io")
	want := "/dev/data-vg/pvc-no-io"
	if got != want {
		t.Errorf("DevicePath = %q; want %q", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Create tests
// ─────────────────────────────────────────────────────────────────────────────

func TestCreate_LinearLV(t *testing.T) {
	t.Parallel()

	// Sequence: lvsBytes → not-exist, lvcreate → ok, lvsBytes → 4GiB
	fake := newFake(t,
		lvNotExistResp("data-vg", "pvc-new"), // existence check: not found
		ok(""),                               // lvcreate succeeds
		ok("4294967296\n"),                   // read-back lv_size = 4 GiB
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	devPath, allocated, err := b.Create(context.Background(), "data-vg/pvc-new", 4<<30, nil)

	if err != nil {
		t.Fatalf("Create (linear): unexpected error: %v", err)
	}
	if allocated != 4<<30 {
		t.Errorf("Create (linear): allocated = %d; want %d", allocated, 4<<30)
	}
	want := "/dev/data-vg/pvc-new"
	if devPath != want {
		t.Errorf("Create (linear): devicePath = %q; want %q", devPath, want)
	}

	fake.assertCallCount(3)
	// First call: lvs existence check
	fake.assertArgsContain(0, "lvs", "data-vg/pvc-new")
	// Second call: lvcreate -n pvc-new -L <size>b data-vg
	fake.assertArgsContain(1, "lvcreate", "-n", "pvc-new", "-L", "4294967296b", "data-vg")
	// Third call: read-back lvs
	fake.assertArgsContain(2, "lvs", "data-vg/pvc-new")
}

func TestCreate_ThinLV(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		lvNotExistResp("data-vg", "pvc-thin"), // existence check: not found
		ok(""),                                // lvcreate succeeds
		ok("2147483648\n"),                    // read-back lv_size = 2 GiB
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	devPath, allocated, err := b.Create(context.Background(), "data-vg/pvc-thin", 2<<30, nil)

	if err != nil {
		t.Fatalf("Create (thin): unexpected error: %v", err)
	}
	if allocated != 2<<30 {
		t.Errorf("Create (thin): allocated = %d; want %d", allocated, 2<<30)
	}
	want := "/dev/data-vg/pvc-thin"
	if devPath != want {
		t.Errorf("Create (thin): devicePath = %q; want %q", devPath, want)
	}

	fake.assertCallCount(3)
	// Second call: lvcreate with --virtualsize (long form) and --thinpool flag.
	// createThinLV uses the long form --virtualsize for clarity in logs.
	fake.assertArgsContain(1, "lvcreate", "-n", "pvc-thin", "--virtualsize", "2147483648b", "--thinpool", "thin-pool-0", "data-vg")
}

func TestCreate_Idempotent_AlreadyExists(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok("1073741824\n"), // lv_size = 1 GiB — volume already exists
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	devPath, allocated, err := b.Create(context.Background(), "data-vg/pvc-existing", 1<<30, nil)

	if err != nil {
		t.Fatalf("Create (idempotent): unexpected error: %v", err)
	}
	if allocated != 1<<30 {
		t.Errorf("Create (idempotent): allocated = %d; want %d", allocated, 1<<30)
	}
	want := "/dev/data-vg/pvc-existing"
	if devPath != want {
		t.Errorf("Create (idempotent): devicePath = %q; want %q", devPath, want)
	}
	fake.assertCallCount(1)
}

func TestCreate_CommandFails(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		lvNotExistResp("data-vg", "pvc-fail"),
		fail("  Insufficient free space: 1024 extents needed, but only 512 available"),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Create(context.Background(), "data-vg/pvc-fail", 1<<30, nil)
	if err == nil {
		t.Fatal("Create: expected error when lvcreate fails")
	}
	if !strings.Contains(err.Error(), "Insufficient free space") {
		t.Errorf("Create: error %q should mention 'Insufficient free space'", err)
	}
}

// TestCreate_InvalidCapacity verifies that Create rejects non-positive
// capacityBytes without making any LVM CLI calls.
func TestCreate_InvalidCapacity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		capacity int64
	}{
		{"zero", 0},
		{"negative", -1},
		{"large negative", -4096},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// No fake responses needed — no LVM calls should be made.
			fake := newFake(t)
			b := lvm.New("data-vg", "")
			lvm.SetBackendExec(t, b, fake.exec())

			_, _, err := b.Create(context.Background(), "data-vg/pvc-bad", tc.capacity, nil)
			if err == nil {
				t.Fatalf("Create (capacity=%d): expected error, got nil", tc.capacity)
			}
			if !strings.Contains(err.Error(), "capacityBytes must be positive") {
				t.Errorf("Create: error %q should mention 'capacityBytes must be positive'", err)
			}
			fake.assertCallCount(0)
		})
	}
}

// TestCreate_EmptyLVName verifies that Create rejects a volumeID that produces
// an empty LV name (e.g. "data-vg/" with nothing after the slash).
func TestCreate_EmptyLVName(t *testing.T) {
	t.Parallel()

	fake := newFake(t)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Create(context.Background(), "data-vg/", 1<<30, nil)
	if err == nil {
		t.Fatal("Create (empty LV name): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "volume name") {
		t.Errorf("Create: error %q should mention 'volume name'", err)
	}
	fake.assertCallCount(0)
}

// TestCreate_VGOverrideMismatch verifies that Create rejects a BackendParams
// whose LVM volume_group does not match the backend VG.
func TestCreate_VGOverrideMismatch(t *testing.T) {
	t.Parallel()

	fake := newFake(t)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	params := &agentv1.BackendParams{
		Params: &agentv1.BackendParams_Lvm{
			Lvm: &agentv1.LvmVolumeParams{
				VolumeGroup: "other-vg", // mismatches backend VG "data-vg"
			},
		},
	}

	_, _, err := b.Create(context.Background(), "data-vg/pvc-vg-mismatch", 1<<30, params)
	if err == nil {
		t.Fatal("Create (VG override mismatch): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "other-vg") || !strings.Contains(err.Error(), "data-vg") {
		t.Errorf("Create: error %q should mention both VG names", err)
	}
	// No LVM calls should be made — validation fails before any I/O.
	fake.assertCallCount(0)
}

// TestCreate_VGOverrideMatch verifies that Create succeeds when BackendParams
// specifies a volume_group that matches the backend VG.
func TestCreate_VGOverrideMatch(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		lvNotExistResp("data-vg", "pvc-vg-ok"), // existence check: not found
		ok(""),                                 // lvcreate succeeds
		ok("1073741824\n"),                     // read-back lv_size = 1 GiB
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	params := &agentv1.BackendParams{
		Params: &agentv1.BackendParams_Lvm{
			Lvm: &agentv1.LvmVolumeParams{
				VolumeGroup: "data-vg", // matches backend VG — OK
			},
		},
	}

	devPath, allocated, err := b.Create(context.Background(), "data-vg/pvc-vg-ok", 1<<30, params)
	if err != nil {
		t.Fatalf("Create (VG override match): unexpected error: %v", err)
	}
	if allocated != 1<<30 {
		t.Errorf("Create: allocated = %d; want %d", allocated, int64(1<<30))
	}
	if devPath != "/dev/data-vg/pvc-vg-ok" {
		t.Errorf("Create: devicePath = %q; want %q", devPath, "/dev/data-vg/pvc-vg-ok")
	}
	fake.assertCallCount(3)
}

// TestCreate_ExtraFlagsLinear verifies that ExtraFlags from LvmVolumeParams are
// forwarded to lvcreate for linear LVs.
func TestCreate_ExtraFlagsLinear(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		lvNotExistResp("data-vg", "pvc-tagged"), // existence check: not found
		ok(""),                                  // lvcreate succeeds
		ok("1073741824\n"),                      // read-back lv_size = 1 GiB
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	params := &agentv1.BackendParams{
		Params: &agentv1.BackendParams_Lvm{
			Lvm: &agentv1.LvmVolumeParams{
				ExtraFlags: []string{"--addtag", "owner=team-a"},
			},
		},
	}

	_, _, err := b.Create(context.Background(), "data-vg/pvc-tagged", 1<<30, params)
	if err != nil {
		t.Fatalf("Create (extra flags linear): unexpected error: %v", err)
	}

	fake.assertCallCount(3)
	// Verify extra flags appear in the lvcreate invocation.
	fake.assertArgsContain(1, "lvcreate", "--addtag", "owner=team-a")
	// VG must still be present as the final argument.
	fake.assertArgsContain(1, "data-vg")
}

// TestCreate_ExtraFlagsThin verifies that ExtraFlags are forwarded to lvcreate
// for thin-provisioned LVs as well.
func TestCreate_ExtraFlagsThin(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		lvNotExistResp("data-vg", "pvc-thin-tagged"), // existence check: not found
		ok(""),             // lvcreate succeeds
		ok("2147483648\n"), // read-back lv_size = 2 GiB
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	params := &agentv1.BackendParams{
		Params: &agentv1.BackendParams_Lvm{
			Lvm: &agentv1.LvmVolumeParams{
				ExtraFlags: []string{"--addtag", "env=prod"},
			},
		},
	}

	_, _, err := b.Create(context.Background(), "data-vg/pvc-thin-tagged", 2<<30, params)
	if err != nil {
		t.Fatalf("Create (extra flags thin): unexpected error: %v", err)
	}

	fake.assertCallCount(3)
	// Verify thin-specific flags AND extra flags appear.
	fake.assertArgsContain(1, "lvcreate", "--thinpool", "thin-pool-0", "--addtag", "env=prod")
}

// TestCreate_ConflictError verifies that Create returns a *backend.ConflictError
// when an LV with the same volumeID already exists but with a different capacity.
// No lvcreate should be called — the conflict is detected during the pre-create
// existence check, before any provisioning work is attempted.
func TestCreate_ConflictError(t *testing.T) {
	t.Parallel()

	const existingBytes = int64(2 << 30)  // 2 GiB already provisioned
	const requestedBytes = int64(4 << 30) // caller requests 4 GiB (different size)

	// Only the existence-check lvs call should happen.
	fake := newFake(t,
		ok(fmt.Sprintf("%d\n", existingBytes)), // LV exists, lv_size = 2 GiB
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Create(context.Background(), "data-vg/pvc-conflict", requestedBytes, nil)
	if err == nil {
		t.Fatal("Create (conflict): expected error, got nil")
	}

	var conflict *backend.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Create (conflict): expected *backend.ConflictError, got %T: %v", err, err)
	}
	if conflict.ExistingBytes != existingBytes {
		t.Errorf("ConflictError.ExistingBytes = %d; want %d", conflict.ExistingBytes, existingBytes)
	}
	if conflict.RequestedBytes != requestedBytes {
		t.Errorf("ConflictError.RequestedBytes = %d; want %d", conflict.RequestedBytes, requestedBytes)
	}
	// Only the pre-check lvs call should have been made; no lvcreate.
	fake.assertCallCount(1)
}

// TestCreate_ConflictError_ThinLV verifies the same conflict detection in thin
// provisioning mode: the pre-check path is the same regardless of whether a
// thin pool is configured.
func TestCreate_ConflictError_ThinLV(t *testing.T) {
	t.Parallel()

	const existingBytes = int64(1 << 30)  // 1 GiB already provisioned
	const requestedBytes = int64(8 << 30) // caller requests 8 GiB

	fake := newFake(t,
		ok(fmt.Sprintf("%d\n", existingBytes)), // LV exists at 1 GiB
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Create(context.Background(), "data-vg/pvc-thin-conflict", requestedBytes, nil)
	if err == nil {
		t.Fatal("Create (thin conflict): expected error, got nil")
	}

	var conflict *backend.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Create (thin conflict): expected *backend.ConflictError, got %T: %v", err, err)
	}
	if conflict.ExistingBytes != existingBytes {
		t.Errorf("ConflictError.ExistingBytes = %d; want %d", conflict.ExistingBytes, existingBytes)
	}
	// No lvcreate should be invoked — conflict is detected before provisioning.
	fake.assertCallCount(1)
}

// TestCreate_PreCheckError verifies that Create propagates errors from the
// pre-create existence check when they are not "not found" errors.
// A permission-denied or I/O error from lvs must be surfaced, not silenced.
func TestCreate_PreCheckError(t *testing.T) {
	t.Parallel()

	// Use an error message that does NOT match isNotExistOutput patterns
	// ("failed to find logical volume", "not found", "no such logical volume"),
	// so the error is treated as a genuine failure rather than a "not exist".
	fake := newFake(t,
		fail("  I/O error reading device metadata for data-vg"),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Create(context.Background(), "data-vg/pvc-permerr", 1<<30, nil)
	if err == nil {
		t.Fatal("Create (pre-check error): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "pre-create existence check") {
		t.Errorf("Create (pre-check error): error %q should mention 'pre-create existence check'", err)
	}
	// Only the pre-check lvs call should have been made; no lvcreate.
	fake.assertCallCount(1)
}

// TestCreate_PreCheckError_ThinLV verifies that thin mode also propagates the
// pre-create existence-check error correctly (same code path, different backend).
func TestCreate_PreCheckError_ThinLV(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		fail("  I/O error reading device metadata for data-vg/thin-pool-0"),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Create(context.Background(), "data-vg/pvc-thin-ioerr", 2<<30, nil)
	if err == nil {
		t.Fatal("Create (thin pre-check error): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "pre-create existence check") {
		t.Errorf("Create (thin pre-check error): error %q should mention 'pre-create existence check'", err)
	}
	fake.assertCallCount(1)
}

// TestCreate_ReadBackFails verifies that Create returns an error when lvcreate
// succeeds but the subsequent lvsBytes read-back call fails.  The caller must
// know whether the allocation actually landed on disk, so this failure must be
// propagated rather than ignored.
func TestCreate_ReadBackFails(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		lvNotExistResp("data-vg", "pvc-readfail"), // existence check: not found
		ok(""), // lvcreate succeeds
		fail("  I/O error reading lv_size for data-vg/pvc-readfail"), // read-back fails
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Create(context.Background(), "data-vg/pvc-readfail", 1<<30, nil)
	if err == nil {
		t.Fatal("Create (read-back fails): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "reading lv_size after create") {
		t.Errorf("Create (read-back fails): error %q should mention 'reading lv_size after create'", err)
	}
	fake.assertCallCount(3)
}

// TestCreate_ReadBackFails_ThinLV verifies the same read-back failure path for
// thin-provisioned LVs: the read-back is performed after createThinLV, not
// after the linear lvcreate path, so both code paths must be covered.
func TestCreate_ReadBackFails_ThinLV(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		lvNotExistResp("data-vg", "pvc-thin-readfail"), // existence check: not found
		ok(""), // lvcreate --virtualsize succeeds
		fail("  I/O error reading lv_size for data-vg/pvc-thin-readfail"), // read-back fails
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Create(context.Background(), "data-vg/pvc-thin-readfail", 2<<30, nil)
	if err == nil {
		t.Fatal("Create (thin read-back fails): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "reading lv_size after create") {
		t.Errorf("Create (thin read-back fails): error %q should mention 'reading lv_size after create'", err)
	}
	fake.assertCallCount(3)
}

// TestCreate_ThinCommandFails verifies that Create propagates thin lvcreate
// failures (e.g. insufficient free space in the thin pool) as errors.
// This complements TestCreate_CommandFails which covers the linear path.
func TestCreate_ThinCommandFails(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		lvNotExistResp("data-vg", "pvc-thin-nospace"),                      // existence check: not found
		fail("  Insufficient free space in thin pool data-vg/thin-pool-0"), // lvcreate fails
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Create(context.Background(), "data-vg/pvc-thin-nospace", 100<<30, nil)
	if err == nil {
		t.Fatal("Create (thin failure): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Insufficient free space") {
		t.Errorf("Create (thin failure): error %q should mention 'Insufficient free space'", err)
	}
	fake.assertCallCount(2)
}

// TestCreate_InvalidVolumeID_TrailingSlash verifies that a volumeID ending with
// a slash (e.g. "data-vg/") is rejected because the extracted LV name is empty.
// This is distinct from TestCreate_EmptyLVName in that it uses the full VG prefix
// form and confirms no LVM CLI calls are made when validation fails early.
func TestCreate_InvalidVolumeID_TrailingSlash(t *testing.T) {
	t.Parallel()

	fake := newFake(t) // no responses expected — validation must fail before any CLI call
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	// "data-vg/" → lvName strips "data-vg/" prefix leaving an empty string.
	_, _, err := b.Create(context.Background(), "data-vg/", 1<<30, nil)
	if err == nil {
		t.Fatal("Create (trailing slash): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "volume name") {
		t.Errorf("Create (trailing slash): error %q should mention 'volume name'", err)
	}
	fake.assertCallCount(0)
}

// ─────────────────────────────────────────────────────────────────────────────
// createThinLV helper tests (white-box via exported CreateThinLV)
// ─────────────────────────────────────────────────────────────────────────────

// TestCreateThinLV_UsesVirtualsize verifies that createThinLV passes the long
// form --virtualsize flag (not the short alias -V) to lvcreate, and that it
// includes --thinpool with the correct pool name and the VG as a positional arg.
func TestCreateThinLV_UsesVirtualsize(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok(""), // lvcreate succeeds
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	err := lvm.CreateThinLV(b, context.Background(), "data-vg", "pvc-thin-helper", "thin-pool-0", 2<<30, nil)
	if err != nil {
		t.Fatalf("CreateThinLV: unexpected error: %v", err)
	}

	fake.assertCallCount(1)
	// Must use --virtualsize (long form), not -V.
	fake.assertArgsContain(0, "lvcreate", "--virtualsize", "2147483648b")
	// Must pass --thinpool and the pool name.
	fake.assertArgsContain(0, "--thinpool", "thin-pool-0")
	// LV name via -n.
	fake.assertArgsContain(0, "-n", "pvc-thin-helper")
	// VG must be the positional argument.
	fake.assertArgsContain(0, "data-vg")
}

// TestCreateThinLV_Idempotent_AlreadyExists verifies that createThinLV returns
// nil (not an error) when lvcreate exits non-zero because the LV already exists.
// This is the extra idempotency guard beyond the existence pre-check in Create.
func TestCreateThinLV_Idempotent_AlreadyExists(t *testing.T) {
	t.Parallel()

	// Simulate lvcreate saying the LV already exists.
	alreadyExistsResp := fakeResponse{
		out: []byte("  Logical volume \"pvc-dup\" already exists in volume group \"data-vg\"."),
		err: errors.New("exit status 5"),
	}
	fake := newFake(t, alreadyExistsResp)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	err := lvm.CreateThinLV(b, context.Background(), "data-vg", "pvc-dup", "thin-pool-0", 1<<30, nil)
	if err != nil {
		t.Fatalf("CreateThinLV (already exists): expected nil for idempotent case, got: %v", err)
	}
	fake.assertCallCount(1)
}

// TestCreateThinLV_ErrorPropagated verifies that lvcreate failures unrelated to
// "already exists" are wrapped and returned to the caller.
func TestCreateThinLV_ErrorPropagated(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		fail("  Insufficient free space in thin pool data-vg/thin-pool-0"),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	err := lvm.CreateThinLV(b, context.Background(), "data-vg", "pvc-nospace", "thin-pool-0", 100<<30, nil)
	if err == nil {
		t.Fatal("CreateThinLV: expected error when lvcreate fails")
	}
	if !strings.Contains(err.Error(), "Insufficient free space") {
		t.Errorf("CreateThinLV: error %q should mention 'Insufficient free space'", err)
	}
	fake.assertCallCount(1)
}

// TestCreateThinLV_ExtraFlagsForwarded verifies that extra flags are passed
// through to lvcreate in the correct position (before the VG argument).
func TestCreateThinLV_ExtraFlagsForwarded(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok(""), // lvcreate succeeds
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	extraFlags := []string{"--addtag", "env=prod", "--addtag", "owner=team-a"}
	err := lvm.CreateThinLV(b, context.Background(), "data-vg", "pvc-tagged", "thin-pool-0", 4<<30, extraFlags)
	if err != nil {
		t.Fatalf("CreateThinLV (extra flags): unexpected error: %v", err)
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "lvcreate", "--virtualsize", "4294967296b", "--thinpool", "thin-pool-0")
	fake.assertArgsContain(0, "--addtag", "env=prod", "--addtag", "owner=team-a")
	// VG must still appear.
	fake.assertArgsContain(0, "data-vg")
}

// TestCreateThinLV_DifferentThinPool verifies that the thinPool argument
// (not b.thinpool) is used in the lvcreate invocation, allowing the helper to
// be called with an explicit pool name.
func TestCreateThinLV_DifferentThinPool(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok(""), // lvcreate succeeds
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	// Call with a pool name that differs from b.thinpool to confirm the
	// parameter is forwarded as-is.
	err := lvm.CreateThinLV(b, context.Background(), "data-vg", "pvc-alt", "thin-pool-alt", 1<<30, nil)
	if err != nil {
		t.Fatalf("CreateThinLV (alt pool): unexpected error: %v", err)
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "--thinpool", "thin-pool-alt")
}

// TestIsAlreadyExistsOutput verifies the predicate that detects "already
// exists" in LVM output, covering different message capitalizations.
func TestIsAlreadyExistsOutput(t *testing.T) {
	t.Parallel()

	positives := []string{
		"Logical volume \"pvc-dup\" already exists in volume group \"data-vg\".",
		"  logical volume pvc-dup ALREADY EXISTS",
		"already exists",
	}
	for _, msg := range positives {
		if !lvm.IsAlreadyExistsOutput([]byte(msg)) {
			t.Errorf("IsAlreadyExistsOutput(%q) = false; want true", msg)
		}
	}

	negatives := []string{
		"Insufficient free space",
		"Failed to find logical volume",
		"Volume group not found",
		"",
	}
	for _, msg := range negatives {
		if lvm.IsAlreadyExistsOutput([]byte(msg)) {
			t.Errorf("IsAlreadyExistsOutput(%q) = true; want false", msg)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Delete tests
// ─────────────────────────────────────────────────────────────────────────────

func TestDelete_ExistingVolume(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok("  Logical volume \"pvc-del\" successfully removed.\n"), // lvremove succeeds
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	if err := b.Delete(context.Background(), "data-vg/pvc-del"); err != nil {
		t.Fatalf("Delete: unexpected error: %v", err)
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "lvremove", "-y", "data-vg/pvc-del")
}

func TestDelete_Idempotent_NotExist(t *testing.T) {
	t.Parallel()

	// lvremove exits non-zero with "Failed to find logical volume" → should be
	// swallowed for idempotency.
	fake := newFake(t,
		lvNotExistResp("data-vg", "pvc-gone"),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	if err := b.Delete(context.Background(), "data-vg/pvc-gone"); err != nil {
		t.Fatalf("Delete (idempotent): expected nil, got: %v", err)
	}
	fake.assertCallCount(1)
}

func TestDelete_Idempotent_NotFoundVariant(t *testing.T) {
	t.Parallel()

	// Another common LVM error message variant for non-existent LV.
	fake := newFake(t,
		fail("  Volume group \"data-vg\" not found"),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	// When the entire VG is gone, the LV certainly doesn't exist either —
	// treat as idempotent success.
	if err := b.Delete(context.Background(), "data-vg/pvc-gone"); err != nil {
		t.Fatalf("Delete (VG not found): expected nil, got: %v", err)
	}
	fake.assertCallCount(1)
}

func TestDelete_OtherError(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		fail("  Logical volume data-vg/pvc-busy is used by another device"),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	err := b.Delete(context.Background(), "data-vg/pvc-busy")
	if err == nil {
		t.Fatal("Delete: expected error for busy LV")
	}
	if !strings.Contains(err.Error(), "is used by another device") {
		t.Errorf("Delete: error %q should mention 'is used by another device'", err)
	}
}

func TestDelete_PassesCorrectArgs(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok(""), // lvremove succeeds
	)
	b := lvm.New("my-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	if err := b.Delete(context.Background(), "my-vg/pvc-test"); err != nil {
		t.Fatalf("Delete: unexpected error: %v", err)
	}

	fake.assertCallCount(1)
	// Verify exact arguments: lvremove -y my-vg/pvc-test
	fake.assertArgsContain(0, "lvremove", "-y", "my-vg/pvc-test")
}

func TestDelete_ThinLV(t *testing.T) {
	t.Parallel()

	// Thin LVs are deleted with the same lvremove command as linear LVs.
	fake := newFake(t,
		ok("  Logical volume \"pvc-thin-del\" successfully removed.\n"),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	if err := b.Delete(context.Background(), "data-vg/pvc-thin-del"); err != nil {
		t.Fatalf("Delete (thin): unexpected error: %v", err)
	}

	fake.assertCallCount(1)
	// Thin and linear LVs use the same deletion command.
	fake.assertArgsContain(0, "lvremove", "-y", "data-vg/pvc-thin-del")
}

// ─────────────────────────────────────────────────────────────────────────────
// Expand tests
// ─────────────────────────────────────────────────────────────────────────────

func TestExpand_Success(t *testing.T) {
	t.Parallel()

	// Sequence: pre-expand lvsBytes (current < requested) → lvextend → lvsBytes (read-back)
	fake := newFake(t,
		ok("4294967296\n"), // pre-expand size check: current = 4 GiB (< requested 8 GiB)
		ok(""),             // lvextend succeeds
		ok("8589934592\n"), // read-back: 8 GiB
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	allocated, err := b.Expand(context.Background(), "data-vg/pvc-grow", 8<<30)
	if err != nil {
		t.Fatalf("Expand: unexpected error: %v", err)
	}
	if allocated != 8<<30 {
		t.Errorf("Expand: allocated = %d; want %d", allocated, 8<<30)
	}

	fake.assertCallCount(3)
	// First call: pre-expand lvsBytes
	fake.assertArgsContain(0, "lvs", "data-vg/pvc-grow")
	// Second call: lvextend
	fake.assertArgsContain(1, "lvextend", "-L", "8589934592b", "data-vg/pvc-grow")
	// Third call: read-back lvsBytes
	fake.assertArgsContain(2, "lvs", "data-vg/pvc-grow")
}

func TestExpand_RoundedUp(t *testing.T) {
	t.Parallel()

	const requested = 3 << 30
	const rounded = 4 << 30 // LVM may round to next extent boundary

	// Sequence: pre-expand lvsBytes (current < requested) → lvextend → lvsBytes (rounded)
	fake := newFake(t,
		ok("2147483648\n"),               // pre-expand: current = 2 GiB (< 3 GiB)
		ok(""),                           // lvextend succeeds
		ok(fmt.Sprintf("%d\n", rounded)), // read-back: 4 GiB (rounded)
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	allocated, err := b.Expand(context.Background(), "data-vg/pvc-round", requested)
	if err != nil {
		t.Fatalf("Expand (rounded): %v", err)
	}
	if allocated != rounded {
		t.Errorf("Expand: allocated = %d; want %d (rounded)", allocated, rounded)
	}
}

// TestExpand_Idempotent_SameSize verifies that Expand is a no-op when the LV
// is already at the requested size, rather than calling lvextend and getting a
// "New size matches existing size" error from LVM.
func TestExpand_Idempotent_SameSize(t *testing.T) {
	t.Parallel()

	const currentSize = int64(8 << 30) // 8 GiB

	// Only the pre-check lvs call should happen; lvextend must NOT be called.
	fake := newFake(t,
		ok(fmt.Sprintf("%d\n", currentSize)), // pre-check: already at requested size
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	allocated, err := b.Expand(context.Background(), "data-vg/pvc-same", currentSize)
	if err != nil {
		t.Fatalf("Expand (idempotent same-size): unexpected error: %v", err)
	}
	if allocated != currentSize {
		t.Errorf("Expand (idempotent): allocated = %d; want %d", allocated, currentSize)
	}
	// Confirm lvextend was never called (only 1 call: lvsBytes pre-check).
	fake.assertCallCount(1)
}

// TestExpand_ShrinkAttempt verifies that Expand returns an error when the
// requested size is smaller than the current LV size.  Shrinking a volume is
// not supported by ExpandVolume; the caller must request a size that is at
// least as large as the current allocation.
func TestExpand_ShrinkAttempt(t *testing.T) {
	t.Parallel()

	const currentSize = int64(16 << 30) // 16 GiB
	const requested = int64(8 << 30)    // 8 GiB — less than current → shrink

	// Only the pre-check lvsBytes call should happen; lvextend must NOT be called.
	fake := newFake(t,
		ok(fmt.Sprintf("%d\n", currentSize)), // pre-check: LV is already larger
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, err := b.Expand(context.Background(), "data-vg/pvc-large", requested)
	if err == nil {
		t.Fatal("Expand (shrink attempt): expected error, got nil")
	}
	// The error message must mention "shrink" so operators understand why the
	// expansion was rejected.
	if !strings.Contains(err.Error(), "shrink") {
		t.Errorf("Expand (shrink attempt): error %q should mention 'shrink'", err)
	}
	// Confirm only the pre-check lvs call was made and lvextend was not invoked.
	fake.assertCallCount(1)
}

// TestExpand_InvalidRequestedBytes verifies that Expand rejects non-positive
// requestedBytes without touching LVM at all.
func TestExpand_InvalidRequestedBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		requested int64
	}{
		{"zero", 0},
		{"negative", -1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// No fake responses needed — no LVM calls should be made.
			fake := newFake(t)
			b := lvm.New("data-vg", "")
			lvm.SetBackendExec(t, b, fake.exec())

			_, err := b.Expand(context.Background(), "data-vg/pvc-bad", tc.requested)
			if err == nil {
				t.Fatalf("Expand (requested=%d): expected error, got nil", tc.requested)
			}
			fake.assertCallCount(0)
		})
	}
}

// TestExpand_LVNotExist verifies that Expand returns an error when the LV
// does not exist (the pre-check lvsBytes call fails with not-found).
func TestExpand_LVNotExist(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		lvNotExistResp("data-vg", "pvc-missing"), // pre-check: LV not found
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, err := b.Expand(context.Background(), "data-vg/pvc-missing", 4<<30)
	if err == nil {
		t.Fatal("Expand (LV not exist): expected error, got nil")
	}
	fake.assertCallCount(1) // only pre-check; no lvextend
}

// TestExpand_CommandFails verifies that an lvextend failure (e.g. no space
// left in the VG) is propagated as an error.
func TestExpand_CommandFails(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok("1073741824\n"), // pre-check: current = 1 GiB (less than requested 2 GiB)
		fail("  Insufficient free space: 256 extents needed, but only 10 available"),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, err := b.Expand(context.Background(), "data-vg/pvc-nospace", 2<<30)
	if err == nil {
		t.Fatal("Expand: expected error when lvextend fails")
	}
	if !strings.Contains(err.Error(), "Insufficient free space") {
		t.Errorf("Expand: error %q should mention 'Insufficient free space'", err)
	}
}

// TestExpand_ThinLV verifies that Expand works identically for thin-provisioned
// LVs — the same lvextend command is used regardless of provisioning mode.
func TestExpand_ThinLV(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok("2147483648\n"), // pre-check: current = 2 GiB (less than requested 4 GiB)
		ok(""),             // lvextend succeeds
		ok("4294967296\n"), // read-back: 4 GiB
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	allocated, err := b.Expand(context.Background(), "data-vg/pvc-thin-grow", 4<<30)
	if err != nil {
		t.Fatalf("Expand (thin LV): unexpected error: %v", err)
	}
	if allocated != 4<<30 {
		t.Errorf("Expand (thin LV): allocated = %d; want %d", allocated, 4<<30)
	}

	fake.assertCallCount(3)
	// Thin LVs use the same lvextend syntax as linear LVs.
	fake.assertArgsContain(1, "lvextend", "-L", "4294967296b", "data-vg/pvc-thin-grow")
}

// ─────────────────────────────────────────────────────────────────────────────
// Capacity tests
// ─────────────────────────────────────────────────────────────────────────────

func TestCapacity_LinearSuccess(t *testing.T) {
	t.Parallel()

	const total = int64(100 << 30)
	const free = int64(50 << 30)

	fake := newFake(t,
		ok(fmt.Sprintf("  %d  %d\n", total, free)),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (linear): unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity: totalBytes = %d; want %d", gotTotal, total)
	}
	if gotFree != free {
		t.Errorf("Capacity: freeBytes = %d; want %d", gotFree, free)
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "vgs", "data-vg")
}

func TestCapacity_LinearVGNotFound(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		fail("  Volume group \"missing-vg\" not found"),
	)
	b := lvm.New("missing-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity: expected error for missing VG")
	}
}

func TestCapacity_ThinSuccess(t *testing.T) {
	t.Parallel()

	// Total pool = 100 GiB, 50% used → available = 50 GiB
	const total = int64(100 << 30)
	const dataPercent = 50.00

	fake := newFake(t,
		ok(fmt.Sprintf("  %d  %.2f\n", total, dataPercent)),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (thin): unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity (thin): totalBytes = %d; want %d", gotTotal, total)
	}

	// With 50% used, available should be approximately 50 GiB
	expectedFree := total - int64(float64(total)*dataPercent/100.0)
	if gotFree != expectedFree {
		t.Errorf("Capacity (thin): freeBytes = %d; want %d", gotFree, expectedFree)
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "lvs", "data-vg/thin-pool-0")
}

func TestCapacity_MalformedOutput(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok("only-one-field\n"),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity: expected error for malformed output")
	}
}

// TestCapacity_ThinCommandArgs verifies that capacityThin issues exactly one
// `lvs` call with the expected fields and the "<vg>/<thinpool>" path argument.
// This ensures the command format that LVM requires is correct.
func TestCapacity_ThinCommandArgs(t *testing.T) {
	t.Parallel()

	const total = int64(200 << 30) // 200 GiB thin pool
	const dataPercent = 10.00      // 10% used

	fake := newFake(t,
		ok(fmt.Sprintf("  %d  %.2f\n", total, dataPercent)),
	)
	b := lvm.New("my-vg", "my-thin-pool")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (thin cmd args): unexpected error: %v", err)
	}

	fake.assertCallCount(1)
	// Must use lvs (not vgs) for thin pool queries.
	fake.assertArgsContain(0, "lvs")
	// Must request lv_size and data_percent fields.
	fake.assertArgsContain(0, "lv_size,data_percent")
	// Must use byte units without suffix.
	fake.assertArgsContain(0, "--units", "b", "--nosuffix")
	// Must target the full "vg/thinpool" path.
	fake.assertArgsContain(0, "my-vg/my-thin-pool")
}

// TestCapacity_ThinSuccess_ZeroPercent verifies that when a thin pool is empty
// (data_percent = 0.00), the available bytes equals the total pool size.
func TestCapacity_ThinSuccess_ZeroPercent(t *testing.T) {
	t.Parallel()

	const total = int64(50 << 30) // 50 GiB pool
	// Pool is completely empty: 0% data used.
	fake := newFake(t,
		ok(fmt.Sprintf("  %d  0.00\n", total)),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (thin 0%%): unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity (thin 0%%): totalBytes = %d; want %d", gotTotal, total)
	}
	// With 0% usage, all space should be free.
	if gotFree != total {
		t.Errorf("Capacity (thin 0%%): availableBytes = %d; want %d (all free)", gotFree, total)
	}

	fake.assertCallCount(1)
}

// TestCapacity_ThinSuccess_FullPool verifies that when a thin pool is completely
// full (data_percent = 100.00), available bytes is 0 (or close to 0 due to
// integer truncation).
func TestCapacity_ThinSuccess_FullPool(t *testing.T) {
	t.Parallel()

	const total = int64(80 << 30) // 80 GiB pool

	// Pool is 100% used — no more space for thin LVs.
	fake := newFake(t,
		ok(fmt.Sprintf("  %d  100.00\n", total)),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (thin 100%%): unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity (thin 100%%): totalBytes = %d; want %d", gotTotal, total)
	}
	// With 100% used, available bytes should be 0.
	// usedBytes = int64(float64(total) * 100.0 / 100.0) = total.
	if gotFree != 0 {
		t.Errorf("Capacity (thin 100%%): availableBytes = %d; want 0 (pool full)", gotFree)
	}

	fake.assertCallCount(1)
}

// TestCapacity_ThinSuccess_DecimalPercent verifies that data_percent values
// with fractional digits (e.g. 25.53%) are parsed correctly and that the
// available-bytes calculation uses floating-point arithmetic.
func TestCapacity_ThinSuccess_DecimalPercent(t *testing.T) {
	t.Parallel()

	// Use variables (not consts) so Go does not attempt compile-time constant
	// folding of the float64 multiplication, which would produce a non-integer
	// constant that cannot be truncated to int64 at compile time.
	total := int64(100 << 30) // 100 GiB
	dataPercent := 25.53      // fractional percent

	fake := newFake(t,
		ok(fmt.Sprintf("  %d  %.2f\n", total, dataPercent)),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (thin decimal%%): unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity (thin decimal%%): totalBytes = %d; want %d", gotTotal, total)
	}

	// Expected: usedBytes = int64(float64(100GiB) * 25.53 / 100.0)
	expectedUsed := int64(float64(total) * dataPercent / 100.0)
	expectedFree := total - expectedUsed
	if gotFree != expectedFree {
		t.Errorf("Capacity (thin decimal%%): availableBytes = %d; want %d", gotFree, expectedFree)
	}

	fake.assertCallCount(1)
}

// TestCapacity_ThinPoolNotFound verifies that when the thin pool LV does not
// exist, Capacity returns an error rather than silently succeeding.  This guards
// against misconfigured agents that reference a thin pool that was never created.
func TestCapacity_ThinPoolNotFound(t *testing.T) {
	t.Parallel()

	// Simulate lvs output when the thin pool LV or VG does not exist.
	fake := newFake(t,
		fail("  Failed to find logical volume \"data-vg/thin-pool-0\""),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity (thin pool not found): expected error, got nil")
	}
	// Error message should identify which VG/pool was queried.
	if !strings.Contains(err.Error(), "data-vg") {
		t.Errorf("Capacity (thin pool not found): error %q should mention VG name", err)
	}

	fake.assertCallCount(1)
}

// TestCapacity_ThinMalformedOutput_OneField verifies that capacityThin returns
// an error when lvs emits only one whitespace-separated field instead of the
// expected two (lv_size and data_percent).
func TestCapacity_ThinMalformedOutput_OneField(t *testing.T) {
	t.Parallel()

	// lvs output contains only the size, missing data_percent.
	fake := newFake(t,
		ok("107374182400\n"),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity (thin malformed one-field): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected lvs output") {
		t.Errorf("Capacity (thin malformed one-field): error %q should mention 'unexpected lvs output'", err)
	}
}

// TestCapacity_ThinMalformedOutput_Empty verifies that an empty lvs output for
// the thin pool query is treated as a parsing error, not a zero-capacity success.
func TestCapacity_ThinMalformedOutput_Empty(t *testing.T) {
	t.Parallel()

	// lvs returns no output (e.g. the pool was somehow filtered out).
	fake := newFake(t,
		ok(""),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity (thin malformed empty): expected error, got nil")
	}
}

// TestCapacity_ThinParseFails_InvalidSize verifies that a non-numeric lv_size
// field in lvs output triggers a descriptive parse error.
func TestCapacity_ThinParseFails_InvalidSize(t *testing.T) {
	t.Parallel()

	// lv_size field is not a valid integer.
	fake := newFake(t,
		ok("  not-a-number  25.00\n"),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity (thin invalid size): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "lv_size") {
		t.Errorf("Capacity (thin invalid size): error %q should mention 'lv_size'", err)
	}
}

// TestCapacity_ThinParseFails_InvalidPercent verifies that a non-numeric
// data_percent field in lvs output triggers a descriptive parse error.
func TestCapacity_ThinParseFails_InvalidPercent(t *testing.T) {
	t.Parallel()

	// data_percent field is not a valid float.
	fake := newFake(t,
		ok(fmt.Sprintf("  %d  bad-percent\n", int64(100<<30))),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity (thin invalid percent): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "data_percent") {
		t.Errorf("Capacity (thin invalid percent): error %q should mention 'data_percent'", err)
	}
}

// TestCapacity_ThinVsLinearDispatch verifies the backend correctly dispatches
// to the thin pool query (lvs) when a thinpool is configured, and to the VG
// query (vgs) when operating in linear mode.  This guards against the dispatch
// logic being accidentally inverted.
func TestCapacity_ThinVsLinearDispatch(t *testing.T) {
	t.Parallel()

	t.Run("thin uses lvs", func(t *testing.T) {
		t.Parallel()

		const total = int64(40 << 30)
		fake := newFake(t,
			ok(fmt.Sprintf("  %d  20.00\n", total)),
		)
		b := lvm.New("data-vg", "thin-pool-0") // thin mode
		lvm.SetBackendExec(t, b, fake.exec())

		if _, _, err := b.Capacity(context.Background()); err != nil {
			t.Fatalf("Capacity (thin dispatch): unexpected error: %v", err)
		}
		// Thin mode must call lvs, not vgs.
		fake.assertArgsContain(0, "lvs")
	})

	t.Run("linear uses vgs", func(t *testing.T) {
		t.Parallel()

		const total = int64(40 << 30)
		const free = int64(20 << 30)
		fake := newFake(t,
			ok(fmt.Sprintf("  %d  %d\n", total, free)),
		)
		b := lvm.New("data-vg", "") // linear mode (no thinpool)
		lvm.SetBackendExec(t, b, fake.exec())

		if _, _, err := b.Capacity(context.Background()); err != nil {
			t.Fatalf("Capacity (linear dispatch): unexpected error: %v", err)
		}
		// Linear mode must call vgs, not lvs.
		fake.assertArgsContain(0, "vgs")
	})
}

// TestCapacity_LinearFullVG verifies that a completely full volume group —
// where vg_free = 0 — is reported correctly: totalBytes equals the VG size
// and availableBytes is 0.  A full VG should not cause an error; the caller
// (GetCapacity RPC) is responsible for deciding whether to surface this as
// an "insufficient capacity" condition to the CO.
func TestCapacity_LinearFullVG(t *testing.T) {
	t.Parallel()

	const total = int64(50 << 30) // 50 GiB VG
	const free = int64(0)         // completely full — no extents available

	fake := newFake(t,
		ok(fmt.Sprintf("  %d  %d\n", total, free)),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (linear full VG): unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity (linear full VG): totalBytes = %d; want %d", gotTotal, total)
	}
	// A full VG has zero available bytes — this must be returned faithfully,
	// not clamped, negated, or converted to an error.
	if gotFree != 0 {
		t.Errorf("Capacity (linear full VG): availableBytes = %d; want 0", gotFree)
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "vgs", "data-vg")
}

// TestCapacity_LinearCommandArgs verifies that capacityLinear issues exactly
// one `vgs` invocation with the correct field selectors, byte units, no-suffix
// flag, and the VG name as the positional argument.  This prevents regressions
// where the command format drifts away from what LVM requires.
func TestCapacity_LinearCommandArgs(t *testing.T) {
	t.Parallel()

	const total = int64(200 << 30) // 200 GiB VG
	const free = int64(100 << 30)  // 100 GiB free

	fake := newFake(t,
		ok(fmt.Sprintf("  %d  %d\n", total, free)),
	)
	b := lvm.New("my-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (linear cmd args): unexpected error: %v", err)
	}

	fake.assertCallCount(1)
	// Must use vgs (not lvs) for linear VG queries.
	fake.assertArgsContain(0, "vgs")
	// Must request vg_size and vg_free fields together.
	fake.assertArgsContain(0, "vg_size,vg_free")
	// Must request byte units without a unit suffix.
	fake.assertArgsContain(0, "--units", "b", "--nosuffix")
	// Must suppress the header row.
	fake.assertArgsContain(0, "--noheadings")
	// Must target the correct VG.
	fake.assertArgsContain(0, "my-vg")
}

// TestCapacity_LinearParseFails_InvalidSize verifies that a non-numeric
// vg_size field in vgs output triggers a descriptive parse error.  If LVM
// emits unexpected output (e.g. after a version upgrade changes the field
// format), the backend must surface the failure rather than silently returning
// a zero or garbage total.
func TestCapacity_LinearParseFails_InvalidSize(t *testing.T) {
	t.Parallel()

	// vg_size field is not a valid integer — vg_free is valid but irrelevant
	// because parsing must fail at the first field.
	fake := newFake(t,
		ok("  not-a-number  53687091200\n"),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity (linear invalid vg_size): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "vg_size") {
		t.Errorf("Capacity (linear invalid vg_size): error %q should mention 'vg_size'", err)
	}
}

// TestCapacity_LinearParseFails_InvalidFree verifies that a non-numeric
// vg_free field in vgs output is caught and reported as a parse error.
// The vg_size field parses successfully but vg_free must still be validated.
func TestCapacity_LinearParseFails_InvalidFree(t *testing.T) {
	t.Parallel()

	// vg_size is a valid integer; vg_free is not.
	fake := newFake(t,
		ok(fmt.Sprintf("  %d  bad-free\n", int64(100<<30))),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity (linear invalid vg_free): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "vg_free") {
		t.Errorf("Capacity (linear invalid vg_free): error %q should mention 'vg_free'", err)
	}
}

// TestCapacity_LinearMalformedOutput_Empty verifies that completely empty
// vgs output is treated as a parsing error rather than a zero-capacity
// success.  An empty response most likely indicates a misconfigured VG name
// or a silent LVM bug, not a legitimately empty VG.
func TestCapacity_LinearMalformedOutput_Empty(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok(""), // vgs returns no output
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity (linear empty output): expected error, got nil")
	}
	// The error must mention which VG was queried so operators can diagnose
	// the problem without looking at the raw command output.
	if !strings.Contains(err.Error(), "data-vg") {
		t.Errorf("Capacity (linear empty output): error %q should mention VG name", err)
	}
}

// TestCapacity_LinearMalformedOutput_TooManyFields verifies that vgs output
// containing more than the expected two fields (vg_size and vg_free) triggers
// a parse error rather than silently reading the first two fields.  This
// prevents silent data corruption if the -o field list is ever changed.
func TestCapacity_LinearMalformedOutput_TooManyFields(t *testing.T) {
	t.Parallel()

	// Three whitespace-separated fields — one too many.
	fake := newFake(t,
		ok("  107374182400  53687091200  extra-field\n"),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity (linear too many fields): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected vgs output") {
		t.Errorf("Capacity (linear too many fields): error %q should mention 'unexpected vgs output'", err)
	}
}

// TestCapacity_ThinNearlyFull verifies that a thin pool with 99.99% usage
// (nearly but not completely full) reports a small but non-negative available
// byte count.  This exercises the integer-truncation path in the available
// bytes calculation: usedBytes = int64(float64(total) * 99.99 / 100.0) must
// be strictly less than total so that availableBytes > 0.
func TestCapacity_ThinNearlyFull(t *testing.T) {
	t.Parallel()

	total := int64(100 << 30) // 100 GiB pool
	dataPercent := 99.99      // 99.99% used — just under completely full

	fake := newFake(t,
		ok(fmt.Sprintf("  %d  %.2f\n", total, dataPercent)),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (thin nearly full): unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity (thin nearly full): totalBytes = %d; want %d", gotTotal, total)
	}

	// usedBytes is integer-truncated so availableBytes must be >= 0.
	if gotFree < 0 {
		t.Errorf("Capacity (thin nearly full): availableBytes = %d; must not be negative", gotFree)
	}

	// With 99.99% used the pool is not 100% full, so integer-truncated
	// availableBytes must be small but positive.
	expectedUsed := int64(float64(total) * dataPercent / 100.0)
	expectedFree := total - expectedUsed
	if gotFree != expectedFree {
		t.Errorf("Capacity (thin nearly full): availableBytes = %d; want %d", gotFree, expectedFree)
	}

	// The pool is nearly full but not empty, so at least some bytes remain.
	if gotFree <= 0 {
		t.Errorf("Capacity (thin nearly full): availableBytes = %d; expected > 0 for 99.99%% used", gotFree)
	}

	fake.assertCallCount(1)
}

// TestCapacity_LinearVGWithMinimalFreeSpace verifies that a VG with only one
// extent free (a very small amount of free space, e.g. 4 MiB in a large VG)
// is reported accurately.  This checks boundary conditions near the "almost
// full" threshold without crossing into the full-VG case.
func TestCapacity_LinearVGWithMinimalFreeSpace(t *testing.T) {
	t.Parallel()

	const total = int64(100 << 30) // 100 GiB VG
	const free = int64(4 << 20)    // only 4 MiB free (one typical LVM extent)

	fake := newFake(t,
		ok(fmt.Sprintf("  %d  %d\n", total, free)),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (linear minimal free): unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity (linear minimal free): totalBytes = %d; want %d", gotTotal, total)
	}
	if gotFree != free {
		t.Errorf("Capacity (linear minimal free): availableBytes = %d; want %d", gotFree, free)
	}

	fake.assertCallCount(1)
}

// TestCapacity_ThinSuccess_ExactlyHalfFull verifies a common boundary: a thin
// pool that is exactly 50% used should report exactly half the total as
// available.  Because 50.00% is exactly representable as a binary fraction
// (0.5), this case avoids floating-point rounding and makes the expected
// value trivially verifiable.
func TestCapacity_ThinSuccess_ExactlyHalfFull(t *testing.T) {
	t.Parallel()

	// Use a power-of-two size so that 50% is exact in binary floating point.
	const total = int64(128 << 30) // 128 GiB pool
	const dataPercent = 50.00

	fake := newFake(t,
		ok(fmt.Sprintf("  %d  %.2f\n", total, dataPercent)),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (thin exactly half full): unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity (thin exactly half full): totalBytes = %d; want %d", gotTotal, total)
	}
	// With exactly 50% used, available should equal exactly 50% of total.
	expectedFree := total / 2
	if gotFree != expectedFree {
		t.Errorf("Capacity (thin exactly half full): availableBytes = %d; want %d", gotFree, expectedFree)
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "lvs", "data-vg/thin-pool-0")
}

// TestCapacity_ThinPoolVGNotFound verifies that when the VG itself is missing
// (the lvs command for the thin pool returns a VG-not-found error), Capacity
// surfaces the error rather than silently returning zero values.  This
// distinguishes a misconfigured thin-mode backend from a healthy empty pool.
func TestCapacity_ThinPoolVGNotFound(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		fail("  Volume group \"missing-vg\" not found"),
	)
	b := lvm.New("missing-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity (thin VG not found): expected error, got nil")
	}
	// Error must identify the VG so the operator can diagnose which backend
	// is misconfigured.
	if !strings.Contains(err.Error(), "missing-vg") {
		t.Errorf("Capacity (thin VG not found): error %q should mention VG name", err)
	}

	fake.assertCallCount(1)
}

// TestCapacity_LinearTabSeparatedOutput verifies that vgs output using tab
// characters as field separators (rather than spaces) is parsed correctly.
// LVM's --noheadings output uses whitespace as the delimiter, and strings.Fields
// handles any Unicode whitespace, so tab-separated output must work identically
// to space-separated output.
func TestCapacity_LinearTabSeparatedOutput(t *testing.T) {
	t.Parallel()

	const total = int64(200 << 30) // 200 GiB VG
	const free = int64(100 << 30)  // 100 GiB free

	// Tab-separated output as LVM sometimes produces when run via scripts.
	fake := newFake(t,
		ok(fmt.Sprintf("\t%d\t%d\n", total, free)),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (linear tab-separated): unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity (linear tab-separated): totalBytes = %d; want %d", gotTotal, total)
	}
	if gotFree != free {
		t.Errorf("Capacity (linear tab-separated): freeBytes = %d; want %d", gotFree, free)
	}

	fake.assertCallCount(1)
}

// TestCapacity_ThinTabSeparatedOutput verifies that lvs output for the thin
// pool using tab characters as field separators is parsed correctly.  This
// guards against regressions where the parser assumes a specific whitespace
// character rather than using strings.Fields (which is whitespace-agnostic).
func TestCapacity_ThinTabSeparatedOutput(t *testing.T) {
	t.Parallel()

	const total = int64(100 << 30) // 100 GiB pool
	const dataPercent = 30.00      // 30% used → 70 GiB available

	// Tab-separated: <lv_size>\t<data_percent>
	fake := newFake(t,
		ok(fmt.Sprintf("\t%d\t%.2f\n", total, dataPercent)),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (thin tab-separated): unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity (thin tab-separated): totalBytes = %d; want %d", gotTotal, total)
	}

	expectedUsed := int64(float64(total) * dataPercent / 100.0)
	expectedFree := total - expectedUsed
	if gotFree != expectedFree {
		t.Errorf("Capacity (thin tab-separated): availableBytes = %d; want %d", gotFree, expectedFree)
	}

	fake.assertCallCount(1)
}

// TestCapacity_ThinOverProvisioned verifies the behaviour when a thin pool
// reports data_percent > 100.  This can occur in LVM when the thin pool
// metadata volume is also partially full and LVM reports combined usage, or
// when the pool has been over-committed and actual written data exceeds the
// nominal pool size.  The backend should return the available bytes even if
// it is negative (or zero), so the caller can decide how to surface the
// over-provisioned state without the backend silently clamping the value.
func TestCapacity_ThinOverProvisioned(t *testing.T) {
	t.Parallel()

	total := int64(50 << 30) // 50 GiB nominal pool size
	dataPercent := 110.00    // 110% used — pool has more written data than capacity

	fake := newFake(t,
		ok(fmt.Sprintf("  %d  %.2f\n", total, dataPercent)),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (thin over-provisioned): unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity (thin over-provisioned): totalBytes = %d; want %d", gotTotal, total)
	}

	// usedBytes = int64(float64(total) * 110.0 / 100.0) > total
	// availableBytes = total - usedBytes < 0
	expectedUsed := int64(float64(total) * dataPercent / 100.0)
	expectedFree := total - expectedUsed // will be negative
	if gotFree != expectedFree {
		t.Errorf("Capacity (thin over-provisioned): availableBytes = %d; want %d", gotFree, expectedFree)
	}

	fake.assertCallCount(1)
}

// TestCapacity_LinearSmallVG verifies accurate reporting for a very small VG
// (e.g. a test VG backed by a loop device with only a few GiB).  Small VGs
// are common in CI environments and must be handled identically to large ones.
func TestCapacity_LinearSmallVG(t *testing.T) {
	t.Parallel()

	const total = int64(1 << 30)      // 1 GiB VG
	const free = int64(512 * 1 << 20) // 512 MiB free

	fake := newFake(t,
		ok(fmt.Sprintf("  %d  %d\n", total, free)),
	)
	b := lvm.New("test-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (linear small VG): unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity (linear small VG): totalBytes = %d; want %d", gotTotal, total)
	}
	if gotFree != free {
		t.Errorf("Capacity (linear small VG): freeBytes = %d; want %d", gotFree, free)
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "vgs", "test-vg")
}

// TestCapacity_VGNameForwardedCorrectly verifies that the VG name provided at
// backend construction time is forwarded verbatim to the vgs and lvs commands.
// This prevents regressions where the VG name is accidentally truncated,
// lowercased, or otherwise transformed before use.
func TestCapacity_VGNameForwardedCorrectly(t *testing.T) {
	t.Parallel()

	const vgName = "My_VG+special.name"
	const total = int64(10 << 30)
	const free = int64(5 << 30)

	fake := newFake(t,
		ok(fmt.Sprintf("  %d  %d\n", total, free)),
	)
	b := lvm.New(vgName, "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (VG name forwarded): unexpected error: %v", err)
	}

	// Verify the exact VG name was forwarded to the vgs command.
	fake.assertArgsContain(0, vgName)
}

// TestCapacity_ThinPoolNameForwardedCorrectly verifies that both the VG name
// and thin pool name are forwarded verbatim and concatenated as "<vg>/<pool>"
// in the lvs argument.  This prevents regressions where the path separator is
// omitted or the components are ordered incorrectly.
func TestCapacity_ThinPoolNameForwardedCorrectly(t *testing.T) {
	t.Parallel()

	const vgName = "my-vg"
	const poolName = "pool0"
	const total = int64(20 << 30)
	const dataPercent = 0.00

	fake := newFake(t,
		ok(fmt.Sprintf("  %d  %.2f\n", total, dataPercent)),
	)
	b := lvm.New(vgName, poolName)
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (thin pool name forwarded): unexpected error: %v", err)
	}

	// The lvs call must use "<vg>/<pool>" as the positional argument.
	fake.assertArgsContain(0, vgName+"/"+poolName)
}

// TestCapacity_ThinSmallPool verifies accurate capacity reporting for a very
// small thin pool (e.g. 100 MiB), which is common in integration test
// environments backed by loop devices.
func TestCapacity_ThinSmallPool(t *testing.T) {
	t.Parallel()

	const total = int64(100 * 1 << 20) // 100 MiB thin pool
	const dataPercent = 75.00          // 75% used → 25 MiB free

	fake := newFake(t,
		ok(fmt.Sprintf("  %d  %.2f\n", total, dataPercent)),
	)
	b := lvm.New("ci-vg", "ci-pool")
	lvm.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity (thin small pool): unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity (thin small pool): totalBytes = %d; want %d", gotTotal, total)
	}

	expectedUsed := int64(float64(total) * dataPercent / 100.0)
	expectedFree := total - expectedUsed
	if gotFree != expectedFree {
		t.Errorf("Capacity (thin small pool): availableBytes = %d; want %d", gotFree, expectedFree)
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "ci-vg/ci-pool")
}

// TestCapacity_ThinMalformedOutput_ThreeFields verifies that capacityThin
// returns an error when lvs emits three whitespace-separated fields instead of
// the expected two (lv_size and data_percent).  This catches regressions where
// the -o field list in the lvs invocation is accidentally extended.
func TestCapacity_ThinMalformedOutput_ThreeFields(t *testing.T) {
	t.Parallel()

	// Three fields: lv_size, data_percent, and an unexpected extra field.
	fake := newFake(t,
		ok(fmt.Sprintf("  %d  50.00  extra-field\n", int64(100<<30))),
	)
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity (thin three fields): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected lvs output") {
		t.Errorf("Capacity (thin three fields): error %q should mention 'unexpected lvs output'", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ListVolumes tests
// ─────────────────────────────────────────────────────────────────────────────

func TestListVolumes_MultipleVolumes(t *testing.T) {
	t.Parallel()

	output := "  pvc-a  4294967296\n  pvc-b  2147483648\n"
	fake := newFake(t, ok(output))
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes: unexpected error: %v", err)
	}
	if len(vols) != 2 {
		t.Fatalf("ListVolumes: got %d volumes; want 2", len(vols))
	}

	if vols[0].GetVolumeId() != "data-vg/pvc-a" {
		t.Errorf("vols[0].VolumeId = %q; want %q", vols[0].GetVolumeId(), "data-vg/pvc-a")
	}
	if vols[0].GetCapacityBytes() != 4<<30 {
		t.Errorf("vols[0].CapacityBytes = %d; want %d", vols[0].GetCapacityBytes(), int64(4<<30))
	}
	if vols[0].GetDevicePath() != "/dev/data-vg/pvc-a" {
		t.Errorf("vols[0].DevicePath = %q; want %q", vols[0].GetDevicePath(), "/dev/data-vg/pvc-a")
	}

	if vols[1].GetVolumeId() != "data-vg/pvc-b" {
		t.Errorf("vols[1].VolumeId = %q; want %q", vols[1].GetVolumeId(), "data-vg/pvc-b")
	}
	if vols[1].GetCapacityBytes() != 2<<30 {
		t.Errorf("vols[1].CapacityBytes = %d; want %d", vols[1].GetCapacityBytes(), int64(2<<30))
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "lvs", "data-vg")
}

func TestListVolumes_EmptyVG(t *testing.T) {
	t.Parallel()

	fake := newFake(t, ok(""))
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes (empty): unexpected error: %v", err)
	}
	if len(vols) != 0 {
		t.Errorf("ListVolumes (empty): got %d volumes; want 0", len(vols))
	}
}

func TestListVolumes_VGNotExist(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		fail("  Volume group \"missing-vg\" not found"),
	)
	b := lvm.New("missing-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes (not exist): expected nil error, got: %v", err)
	}
	if len(vols) != 0 {
		t.Errorf("ListVolumes (not exist): got %d volumes; want 0", len(vols))
	}
}

func TestListVolumes_CommandError(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		fail("permission denied"),
	)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, err := b.ListVolumes(context.Background())
	if err == nil {
		t.Fatal("ListVolumes (error): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("ListVolumes (error): error %q should mention 'permission denied'", err)
	}
}

func TestListVolumes_ThinMode_SkipsThinPool(t *testing.T) {
	t.Parallel()

	// In thin mode, the thin pool LV itself shows up in lvs output; it should
	// be excluded from the returned volume list.
	output := "  thin-pool-0  107374182400\n  pvc-a  4294967296\n"
	fake := newFake(t, ok(output))
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes (thin, skip pool): unexpected error: %v", err)
	}
	if len(vols) != 1 {
		t.Fatalf("ListVolumes (thin, skip pool): got %d volumes; want 1", len(vols))
	}
	if vols[0].GetVolumeId() != "data-vg/pvc-a" {
		t.Errorf("vols[0].VolumeId = %q; want %q", vols[0].GetVolumeId(), "data-vg/pvc-a")
	}
}

func TestListVolumes_SingleVolume(t *testing.T) {
	t.Parallel()

	// A VG containing exactly one LV should return a slice with one entry.
	output := "  pvc-only  1073741824\n"
	fake := newFake(t, ok(output))
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes (single): unexpected error: %v", err)
	}
	if len(vols) != 1 {
		t.Fatalf("ListVolumes (single): got %d volumes; want 1", len(vols))
	}
	if vols[0].GetVolumeId() != "data-vg/pvc-only" {
		t.Errorf("VolumeId = %q; want %q", vols[0].GetVolumeId(), "data-vg/pvc-only")
	}
	if vols[0].GetCapacityBytes() != 1<<30 {
		t.Errorf("CapacityBytes = %d; want %d", vols[0].GetCapacityBytes(), int64(1<<30))
	}
	if vols[0].GetDevicePath() != "/dev/data-vg/pvc-only" {
		t.Errorf("DevicePath = %q; want %q", vols[0].GetDevicePath(), "/dev/data-vg/pvc-only")
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "lvs", "data-vg")
}

// TestListVolumes_SingleVolume_NoTrailingNewline mirrors the ZFS test of the
// same name: lvs output that lacks a trailing newline must still be parsed
// correctly into volume metadata.  Some LVM versions omit the final newline
// when there is exactly one row of output.
func TestListVolumes_SingleVolume_NoTrailingNewline(t *testing.T) {
	t.Parallel()

	// Deliberately no trailing newline — the parser must handle this.
	output := "  pvc-123  536870912"
	fake := newFake(t, ok(output))
	b := lvm.New("pool-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes (no newline): unexpected error: %v", err)
	}
	if len(vols) != 1 {
		t.Fatalf("ListVolumes (no newline): got %d volumes; want 1", len(vols))
	}
	if vols[0].GetVolumeId() != "pool-vg/pvc-123" {
		t.Errorf("VolumeId = %q; want %q", vols[0].GetVolumeId(), "pool-vg/pvc-123")
	}
	if vols[0].GetCapacityBytes() != 512<<20 {
		t.Errorf("CapacityBytes = %d; want %d", vols[0].GetCapacityBytes(), int64(512<<20))
	}
}

// TestListVolumes_CommandArgs verifies that ListVolumes passes the expected
// flags to lvs so that the output is machine-parseable: no column headings,
// byte units with no suffix.  This mirrors the pattern established in the ZFS
// test suite where argument construction is verified independently of output
// parsing.
func TestListVolumes_CommandArgs(t *testing.T) {
	t.Parallel()

	fake := newFake(t, ok(""))
	b := lvm.New("my-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes (args check): unexpected error: %v", err)
	}

	fake.assertCallCount(1)
	// Verify all required flags are present.
	fake.assertArgsContain(0, "lvs", "--noheadings", "-o", "lv_name,lv_size",
		"--units", "b", "--nosuffix", "my-vg")
}

// TestListVolumes_MalformedLine_TooManyFields verifies that a lvs output line
// containing more than two whitespace-separated tokens causes ListVolumes to
// return an error rather than silently producing incorrect volume metadata.
func TestListVolumes_MalformedLine_TooManyFields(t *testing.T) {
	t.Parallel()

	// Three fields instead of the expected two.
	output := "  pvc-a  4294967296  extra-field\n"
	fake := newFake(t, ok(output))
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, err := b.ListVolumes(context.Background())
	if err == nil {
		t.Fatal("ListVolumes (malformed line): expected error, got nil")
	}
}

// TestListVolumes_NonNumericSize verifies that a non-numeric lv_size field in
// lvs output causes a parse error to be returned.  This prevents silent
// production of zero-byte volumes when unexpected output format is encountered.
func TestListVolumes_NonNumericSize(t *testing.T) {
	t.Parallel()

	// Human-readable size instead of raw bytes (e.g. if --nosuffix was not
	// respected by some LVM versions).
	output := "  pvc-a  4.00g\n"
	fake := newFake(t, ok(output))
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	_, err := b.ListVolumes(context.Background())
	if err == nil {
		t.Fatal("ListVolumes (non-numeric size): expected parse error, got nil")
	}
}

// TestListVolumes_ThinMode_CommandArgs verifies that ListVolumes in thin mode
// still targets the VG (not the thin pool) when invoking lvs, because the VG
// is the scope that owns all LVs including the thin pool and its volumes.
func TestListVolumes_ThinMode_CommandArgs(t *testing.T) {
	t.Parallel()

	fake := newFake(t, ok(""))
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	_, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes (thin, args): unexpected error: %v", err)
	}

	fake.assertCallCount(1)
	// The VG name — not the thin pool — must be the final argument.
	fake.assertArgsContain(0, "lvs", "data-vg")
}

// TestListVolumes_ThinMode_MultipleVolumes verifies that a thin-provisioned VG
// containing multiple data volumes (and the thin pool LV itself) returns only
// the data volumes, each with the correct volumeID and device path.
func TestListVolumes_ThinMode_MultipleVolumes(t *testing.T) {
	t.Parallel()

	// lvs returns the thin pool entry and two thin LVs; pool entry must be
	// filtered out.
	output := "  thin-pool-0  107374182400\n  pvc-a  4294967296\n  pvc-b  2147483648\n"
	fake := newFake(t, ok(output))
	b := lvm.New("data-vg", "thin-pool-0")
	lvm.SetBackendExec(t, b, fake.exec())

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes (thin multi): unexpected error: %v", err)
	}
	if len(vols) != 2 {
		t.Fatalf("ListVolumes (thin multi): got %d volumes; want 2", len(vols))
	}

	if vols[0].GetVolumeId() != "data-vg/pvc-a" {
		t.Errorf("vols[0].VolumeId = %q; want %q", vols[0].GetVolumeId(), "data-vg/pvc-a")
	}
	if vols[0].GetCapacityBytes() != 4<<30 {
		t.Errorf("vols[0].CapacityBytes = %d; want %d", vols[0].GetCapacityBytes(), int64(4<<30))
	}
	if vols[0].GetDevicePath() != "/dev/data-vg/pvc-a" {
		t.Errorf("vols[0].DevicePath = %q; want %q", vols[0].GetDevicePath(), "/dev/data-vg/pvc-a")
	}

	if vols[1].GetVolumeId() != "data-vg/pvc-b" {
		t.Errorf("vols[1].VolumeId = %q; want %q", vols[1].GetVolumeId(), "data-vg/pvc-b")
	}
	if vols[1].GetCapacityBytes() != 2<<30 {
		t.Errorf("vols[1].CapacityBytes = %d; want %d", vols[1].GetCapacityBytes(), int64(2<<30))
	}
	if vols[1].GetDevicePath() != "/dev/data-vg/pvc-b" {
		t.Errorf("vols[1].DevicePath = %q; want %q", vols[1].GetDevicePath(), "/dev/data-vg/pvc-b")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ProvisionMode tests
// ─────────────────────────────────────────────────────────────────────────────

func TestProvisionMode_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mode lvm.ProvisionMode
		want string
	}{
		{lvm.ProvisionModeLinear, "linear"},
		{lvm.ProvisionModeThin, "thin"},
	}
	for _, tc := range tests {
		got := tc.mode.String()
		if got != tc.want {
			t.Errorf("ProvisionMode(%d).String() = %q; want %q", int(tc.mode), got, tc.want)
		}
	}
}

func TestBackend_Mode_Linear(t *testing.T) {
	t.Parallel()

	b := lvm.New("data-vg", "")
	if got := b.Mode(); got != lvm.ProvisionModeLinear {
		t.Errorf("Mode() = %v; want %v", got, lvm.ProvisionModeLinear)
	}
}

func TestBackend_Mode_Thin(t *testing.T) {
	t.Parallel()

	b := lvm.New("data-vg", "thin-pool-0")
	if got := b.Mode(); got != lvm.ProvisionModeThin {
		t.Errorf("Mode() = %v; want %v", got, lvm.ProvisionModeThin)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Backend accessor tests
// ─────────────────────────────────────────────────────────────────────────────

func TestBackend_VG(t *testing.T) {
	t.Parallel()

	b := lvm.New("my-volume-group", "")
	if got := b.VG(); got != "my-volume-group" {
		t.Errorf("VG() = %q; want %q", got, "my-volume-group")
	}
}

func TestBackend_ThinPool_Empty(t *testing.T) {
	t.Parallel()

	b := lvm.New("data-vg", "")
	if got := b.ThinPool(); got != "" {
		t.Errorf("ThinPool() = %q; want empty string", got)
	}
}

func TestBackend_ThinPool_Set(t *testing.T) {
	t.Parallel()

	b := lvm.New("data-vg", "my-pool")
	if got := b.ThinPool(); got != "my-pool" {
		t.Errorf("ThinPool() = %q; want %q", got, "my-pool")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Validate tests
// ─────────────────────────────────────────────────────────────────────────────

func TestValidate_LinearOK(t *testing.T) {
	t.Parallel()

	b := lvm.New("data-vg", "")
	if err := b.Validate(); err != nil {
		t.Errorf("Validate() linear: unexpected error: %v", err)
	}
}

func TestValidate_ThinOK(t *testing.T) {
	t.Parallel()

	b := lvm.New("data-vg", "thin-pool-0")
	if err := b.Validate(); err != nil {
		t.Errorf("Validate() thin: unexpected error: %v", err)
	}
}

func TestValidate_EmptyVG(t *testing.T) {
	t.Parallel()

	b := lvm.New("", "")
	if err := b.Validate(); err == nil {
		t.Error("Validate(): expected error for empty VG, got nil")
	}
}

func TestValidate_BlankVG(t *testing.T) {
	t.Parallel()

	b := lvm.New("   ", "")
	if err := b.Validate(); err == nil {
		t.Error("Validate(): expected error for blank VG, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ParseParams tests
// ─────────────────────────────────────────────────────────────────────────────

func TestParseParams_Nil(t *testing.T) {
	t.Parallel()

	p := lvm.ParseParams(nil)
	if p.VGOverride != "" {
		t.Errorf("ParseParams(nil).VGOverride = %q; want empty", p.VGOverride)
	}
	if len(p.ExtraFlags) != 0 {
		t.Errorf("ParseParams(nil).ExtraFlags = %v; want empty", p.ExtraFlags)
	}
	if p.AccessType != agentv1.VolumeAccessType_VOLUME_ACCESS_TYPE_UNSPECIFIED {
		t.Errorf("ParseParams(nil).AccessType = %v; want UNSPECIFIED", p.AccessType)
	}
}

func TestParseParams_WithVolumeGroup(t *testing.T) {
	t.Parallel()

	proto := &agentv1.LvmVolumeParams{
		VolumeGroup: "my-vg",
	}
	p := lvm.ParseParams(proto)
	if p.VGOverride != "my-vg" {
		t.Errorf("ParseParams.VGOverride = %q; want %q", p.VGOverride, "my-vg")
	}
}

func TestParseParams_WithVolumeGroupWhitespace(t *testing.T) {
	t.Parallel()

	proto := &agentv1.LvmVolumeParams{
		VolumeGroup: "  data-vg  ",
	}
	p := lvm.ParseParams(proto)
	// Whitespace should be trimmed.
	if p.VGOverride != "data-vg" {
		t.Errorf("ParseParams.VGOverride = %q; want %q (trimmed)", p.VGOverride, "data-vg")
	}
}

func TestParseParams_WithExtraFlags(t *testing.T) {
	t.Parallel()

	proto := &agentv1.LvmVolumeParams{
		ExtraFlags: []string{"--addtag", "owner=team-a"},
	}
	p := lvm.ParseParams(proto)
	if len(p.ExtraFlags) != 2 {
		t.Fatalf("ParseParams.ExtraFlags len = %d; want 2", len(p.ExtraFlags))
	}
	if p.ExtraFlags[0] != "--addtag" || p.ExtraFlags[1] != "owner=team-a" {
		t.Errorf("ParseParams.ExtraFlags = %v; want [--addtag owner=team-a]", p.ExtraFlags)
	}
}

func TestParseParams_EmptyExtraFlags(t *testing.T) {
	t.Parallel()

	proto := &agentv1.LvmVolumeParams{
		VolumeGroup: "data-vg",
		ExtraFlags:  nil,
	}
	p := lvm.ParseParams(proto)
	if len(p.ExtraFlags) != 0 {
		t.Errorf("ParseParams.ExtraFlags = %v; want empty", p.ExtraFlags)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ValidateParams tests
// ─────────────────────────────────────────────────────────────────────────────

func TestValidateParams_NoOverride(t *testing.T) {
	t.Parallel()

	p := lvm.Params{} // no VGOverride
	if err := lvm.ValidateParams(p, "data-vg", ""); err != nil {
		t.Errorf("ValidateParams(empty override): unexpected error: %v", err)
	}
}

func TestValidateParams_MatchingOverride(t *testing.T) {
	t.Parallel()

	p := lvm.Params{VGOverride: "data-vg"}
	if err := lvm.ValidateParams(p, "data-vg", ""); err != nil {
		t.Errorf("ValidateParams(matching override): unexpected error: %v", err)
	}
}

func TestValidateParams_MismatchedOverride(t *testing.T) {
	t.Parallel()

	p := lvm.Params{VGOverride: "other-vg"}
	err := lvm.ValidateParams(p, "data-vg", "")
	if err == nil {
		t.Fatal("ValidateParams(mismatched override): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "other-vg") || !strings.Contains(err.Error(), "data-vg") {
		t.Errorf("ValidateParams: error %q should mention both VG names", err)
	}
}

func TestValidateParams_ExtraFlagsAlwaysOK(t *testing.T) {
	t.Parallel()

	p := lvm.Params{ExtraFlags: []string{"--addtag", "k=v"}}
	if err := lvm.ValidateParams(p, "any-vg", ""); err != nil {
		t.Errorf("ValidateParams(extra flags only): unexpected error: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ParseProvisionMode tests
// ─────────────────────────────────────────────────────────────────────────────

func TestParseProvisionMode_Linear(t *testing.T) {
	t.Parallel()

	mode, ok := lvm.ParseProvisionMode("linear")
	if !ok {
		t.Fatal("ParseProvisionMode(\"linear\"): expected ok=true")
	}
	if mode != lvm.ProvisionModeLinear {
		t.Errorf("ParseProvisionMode(\"linear\") = %v; want ProvisionModeLinear", mode)
	}
}

func TestParseProvisionMode_Thin(t *testing.T) {
	t.Parallel()

	mode, ok := lvm.ParseProvisionMode("thin")
	if !ok {
		t.Fatal("ParseProvisionMode(\"thin\"): expected ok=true")
	}
	if mode != lvm.ProvisionModeThin {
		t.Errorf("ParseProvisionMode(\"thin\") = %v; want ProvisionModeThin", mode)
	}
}

func TestParseProvisionMode_CaseInsensitive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  lvm.ProvisionMode
	}{
		{"LINEAR", lvm.ProvisionModeLinear},
		{"Linear", lvm.ProvisionModeLinear},
		{"THIN", lvm.ProvisionModeThin},
		{"Thin", lvm.ProvisionModeThin},
		{"  thin  ", lvm.ProvisionModeThin},
		{"  linear  ", lvm.ProvisionModeLinear},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			mode, ok := lvm.ParseProvisionMode(tc.input)
			if !ok {
				t.Fatalf("ParseProvisionMode(%q): expected ok=true", tc.input)
			}
			if mode != tc.want {
				t.Errorf("ParseProvisionMode(%q) = %v; want %v", tc.input, mode, tc.want)
			}
		})
	}
}

func TestParseProvisionMode_Empty(t *testing.T) {
	t.Parallel()

	_, ok := lvm.ParseProvisionMode("")
	if ok {
		t.Error("ParseProvisionMode(\"\"): expected ok=false for empty string")
	}
}

func TestParseProvisionMode_Unknown(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"stripe", "raid", "unknown", "snapshot"} {
		s := s
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			_, ok := lvm.ParseProvisionMode(s)
			if ok {
				t.Errorf("ParseProvisionMode(%q): expected ok=false for unknown mode", s)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ParseParams provision_mode tests
// ─────────────────────────────────────────────────────────────────────────────

func TestParseParams_ProvisionModeLinear(t *testing.T) {
	t.Parallel()

	proto := &agentv1.LvmVolumeParams{ProvisionMode: "linear"}
	p := lvm.ParseParams(proto)
	if !p.HasModeOverride() {
		t.Fatal("ParseParams(provision_mode=linear): HasModeOverride() = false; want true")
	}
	if p.ProvisionModeOverride != lvm.ProvisionModeLinear {
		t.Errorf("ParseParams.ProvisionModeOverride = %v; want ProvisionModeLinear", p.ProvisionModeOverride)
	}
}

func TestParseParams_ProvisionModeThin(t *testing.T) {
	t.Parallel()

	proto := &agentv1.LvmVolumeParams{ProvisionMode: "thin"}
	p := lvm.ParseParams(proto)
	if !p.HasModeOverride() {
		t.Fatal("ParseParams(provision_mode=thin): HasModeOverride() = false; want true")
	}
	if p.ProvisionModeOverride != lvm.ProvisionModeThin {
		t.Errorf("ParseParams.ProvisionModeOverride = %v; want ProvisionModeThin", p.ProvisionModeOverride)
	}
}

func TestParseParams_ProvisionModeEmpty(t *testing.T) {
	t.Parallel()

	proto := &agentv1.LvmVolumeParams{ProvisionMode: ""}
	p := lvm.ParseParams(proto)
	if p.HasModeOverride() {
		t.Error("ParseParams(provision_mode=empty): HasModeOverride() = true; want false")
	}
}

func TestParseParams_ProvisionModeUnknown_NoOverride(t *testing.T) {
	t.Parallel()

	// Unknown provision_mode values produce no override (hasModeOverride=false).
	// The raw string validation (validateProvisionModeString) happens upstream
	// in parseCreateRequest/Create — ParseParams itself just ignores unknowns.
	proto := &agentv1.LvmVolumeParams{ProvisionMode: "stripe"}
	p := lvm.ParseParams(proto)
	if p.HasModeOverride() {
		t.Error("ParseParams(provision_mode=stripe): HasModeOverride() = true; want false (unknown value)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ValidateParams provision_mode tests
// ─────────────────────────────────────────────────────────────────────────────

func TestValidateParams_ThinMode_NoThinPool(t *testing.T) {
	t.Parallel()

	// Requesting thin mode on a backend with no thinpool should fail.
	p := lvm.Params{
		ProvisionModeOverride: lvm.ProvisionModeThin,
	}
	lvm.SetParamsHasModeOverride(t, &p, true)
	err := lvm.ValidateParams(p, "data-vg", "") // no thinpool
	if err == nil {
		t.Fatal("ValidateParams(thin mode, no thinpool): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "thin") {
		t.Errorf("ValidateParams: error %q should mention 'thin'", err)
	}
}

func TestValidateParams_ThinMode_WithThinPool(t *testing.T) {
	t.Parallel()

	p := lvm.Params{
		ProvisionModeOverride: lvm.ProvisionModeThin,
	}
	lvm.SetParamsHasModeOverride(t, &p, true)
	if err := lvm.ValidateParams(p, "data-vg", "thin-pool-0"); err != nil {
		t.Errorf("ValidateParams(thin mode, has thinpool): unexpected error: %v", err)
	}
}

func TestValidateParams_LinearMode_WithThinPool(t *testing.T) {
	t.Parallel()

	p := lvm.Params{
		ProvisionModeOverride: lvm.ProvisionModeLinear,
	}
	lvm.SetParamsHasModeOverride(t, &p, true)
	// Linear mode is valid regardless of thinpool presence.
	if err := lvm.ValidateParams(p, "data-vg", "thin-pool-0"); err != nil {
		t.Errorf("ValidateParams(linear override, has thinpool): unexpected error: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Create provision_mode override integration tests
// ─────────────────────────────────────────────────────────────────────────────

// TestCreate_LinearOverride_OnThinBackend verifies that passing provision_mode
// "linear" forces a linear LV even when the backend has a thinpool configured.
func TestCreate_LinearOverride_OnThinBackend(t *testing.T) {
	t.Parallel()

	// Backend is configured with a thinpool, but the request overrides mode to
	// "linear".  Expect: linear lvcreate (no --virtualsize, no --thinpool).
	fake := newFake(t,
		lvNotExistResp("data-vg", "pvc-linear-override"), // existence check
		ok(""),             // lvcreate (linear) succeeds
		ok("2147483648\n"), // read-back lv_size = 2 GiB
	)
	b := lvm.New("data-vg", "thin-pool-0") // backend default = thin
	lvm.SetBackendExec(t, b, fake.exec())

	params := &agentv1.BackendParams{
		Params: &agentv1.BackendParams_Lvm{
			Lvm: &agentv1.LvmVolumeParams{
				ProvisionMode: "linear", // override: force linear
			},
		},
	}

	devPath, allocated, err := b.Create(context.Background(), "data-vg/pvc-linear-override", 2<<30, params)
	if err != nil {
		t.Fatalf("Create (linear override): unexpected error: %v", err)
	}
	if allocated != 2<<30 {
		t.Errorf("Create (linear override): allocated = %d; want %d", allocated, int64(2<<30))
	}
	if devPath != "/dev/data-vg/pvc-linear-override" {
		t.Errorf("Create (linear override): devicePath = %q; want /dev/data-vg/pvc-linear-override", devPath)
	}

	fake.assertCallCount(3)
	// The second call (index 1) must be a linear lvcreate: uses -L flag (not
	// --virtualsize) and must NOT contain --thinpool.
	fake.assertArgsContain(1, "lvcreate", "-L", "2147483648b")
	allArgs1 := strings.Join(append([]string{fake.CallName(1)}, fake.CallArgs(1)...), " ")
	if strings.Contains(allArgs1, "--virtualsize") {
		t.Error("Create (linear override): expected no --virtualsize flag in lvcreate call")
	}
	if strings.Contains(allArgs1, "--thinpool") {
		t.Error("Create (linear override): expected no --thinpool flag in lvcreate call")
	}
}

// TestCreate_ThinOverride_NoThinPool verifies that passing provision_mode
// "thin" on a backend that has no thinpool configured returns an error.
func TestCreate_ThinOverride_NoThinPool(t *testing.T) {
	t.Parallel()

	fake := newFake(t) // no responses — validation should fail before any LVM call
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	params := &agentv1.BackendParams{
		Params: &agentv1.BackendParams_Lvm{
			Lvm: &agentv1.LvmVolumeParams{
				ProvisionMode: "thin", // override: force thin — but no thinpool!
			},
		},
	}

	_, _, err := b.Create(context.Background(), "data-vg/pvc-thin-fail", 1<<30, params)
	if err == nil {
		t.Fatal("Create (thin override, no thinpool): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "thin") {
		t.Errorf("Create: error %q should mention 'thin'", err)
	}
	fake.assertCallCount(0) // no LVM calls
}

// TestCreate_UnknownProvisionMode rejects unknown provision_mode strings before
// any LVM calls are made.
func TestCreate_UnknownProvisionMode(t *testing.T) {
	t.Parallel()

	fake := newFake(t)
	b := lvm.New("data-vg", "")
	lvm.SetBackendExec(t, b, fake.exec())

	params := &agentv1.BackendParams{
		Params: &agentv1.BackendParams_Lvm{
			Lvm: &agentv1.LvmVolumeParams{
				ProvisionMode: "stripe", // unknown
			},
		},
	}

	_, _, err := b.Create(context.Background(), "data-vg/pvc-bad-mode", 1<<30, params)
	if err == nil {
		t.Fatal("Create (unknown provision_mode): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "stripe") {
		t.Errorf("Create: error %q should mention 'stripe'", err)
	}
	fake.assertCallCount(0)
}

// TestCreate_ThinOverride_WithThinPool verifies that provision_mode "thin"
// passes through when the backend has a thinpool, using the thin lvcreate path.
func TestCreate_ThinOverride_WithThinPool(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		lvNotExistResp("data-vg", "pvc-thin-ok"), // existence check
		ok(""),                                   // lvcreate (thin) succeeds
		ok("1073741824\n"),                       // read-back lv_size = 1 GiB
	)
	b := lvm.New("data-vg", "thin-pool-0") // backend default already = thin
	lvm.SetBackendExec(t, b, fake.exec())

	params := &agentv1.BackendParams{
		Params: &agentv1.BackendParams_Lvm{
			Lvm: &agentv1.LvmVolumeParams{
				ProvisionMode: "thin", // explicit thin (matches backend default)
			},
		},
	}

	_, _, err := b.Create(context.Background(), "data-vg/pvc-thin-ok", 1<<30, params)
	if err != nil {
		t.Fatalf("Create (thin override with thinpool): unexpected error: %v", err)
	}
	fake.assertCallCount(3)
	// Second call must use --virtualsize (thin path).
	fake.assertArgsContain(1, "--virtualsize", "--thinpool", "thin-pool-0")
}

// ─────────────────────────────────────────────────────────────────────────────
// Backend.Type test
// ─────────────────────────────────────────────────────────────────────────────

func TestType_IsLVM(t *testing.T) {
	t.Parallel()

	b := lvm.New("data-vg", "")
	if got := b.Type(); got != agentv1.BackendType_BACKEND_TYPE_LVM {
		t.Errorf("Type() = %v; want BACKEND_TYPE_LVM", got)
	}
}
