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

package zfs_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent/backend/zfs"
)

// Test helpers.

// call records one invocation of the fake executor.
type call struct {
	name string
	args []string
}

// fakeExec is a sequential test double for the ZFS executor.  Each entry in
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

// ok is a helper for a successful fake response with the given output.
func ok(out string) fakeResponse { return fakeResponse{out: []byte(out)} }

// fail is a helper for a failed fake response with the given stderr output.
func fail(out string) fakeResponse {
	return fakeResponse{out: []byte(out), err: errors.New("exit status 1")}
}

// notExistResp simulates the ZFS "dataset does not exist" error output.
func notExistResp(ds string) fakeResponse {
	return fail(fmt.Sprintf("cannot open '%s': dataset does not exist", ds))
}

// DevicePath tests (no executor needed).

func TestDevicePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		pool          string
		parentDataset string
		volumeID      string
		want          string
	}{
		{
			name:          "pool with parent dataset",
			pool:          "hot-data",
			parentDataset: "k8s",
			volumeID:      "hot-data/pvc-abc123",
			want:          "/dev/zvol/hot-data/k8s/pvc-abc123",
		},
		{
			name:          "pool without parent dataset",
			pool:          "tank",
			parentDataset: "",
			volumeID:      "tank/pvc-xyz",
			want:          "/dev/zvol/tank/pvc-xyz",
		},
		{
			name:          "single-letter pool no parent",
			pool:          "z",
			parentDataset: "",
			volumeID:      "z/vol0",
			want:          "/dev/zvol/z/vol0",
		},
		{
			name:          "nested parent dataset",
			pool:          "data",
			parentDataset: "prod/k8s",
			volumeID:      "data/pvc-deep",
			want:          "/dev/zvol/data/prod/k8s/pvc-deep",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := zfs.New(tc.pool, tc.parentDataset)
			got := b.DevicePath(tc.volumeID)
			if got != tc.want {
				t.Errorf("DevicePath(%q) = %q; want %q", tc.volumeID, got, tc.want)
			}
		})
	}
}

// TestDevicePathCustomBase verifies that the per-instance devZvolBase can be
// overridden without affecting any other concurrently-running test.
func TestDevicePathCustomBase(t *testing.T) {
	t.Parallel()

	b := zfs.New("pool", "ns")
	// Override the per-instance base; this is safe in parallel tests.
	zfs.SetBackendDevZvolBase(t, b, "/tmp/fakevol")

	got := b.DevicePath("pool/pvc-1")
	want := "/tmp/fakevol/pool/ns/pvc-1"
	if got != want {
		t.Errorf("DevicePath with custom base = %q; want %q", got, want)
	}
}

// Create tests.

func TestCreate_NewVolume(t *testing.T) {
	t.Parallel()

	// Sequence: volsize-get → not-exist, zfs create → ok, volsize-get → 4GiB
	fake := newFake(t,
		notExistResp("tank/k8s/pvc-new"), // existence check: not found
		ok(""),                           // zfs create -V … succeeds
		ok("4294967296\n"),               // read-back volsize = 4 GiB
	)
	b := zfs.New("tank", "k8s")
	zfs.SetBackendExec(t, b, fake.exec())

	devPath, allocated, err := b.Create(context.Background(), "tank/pvc-new", 4<<30, nil)

	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if allocated != 4<<30 {
		t.Errorf("Create: allocated = %d; want %d", allocated, 4<<30)
	}
	want := "/dev/zvol/tank/k8s/pvc-new"
	if devPath != want {
		t.Errorf("Create: devicePath = %q; want %q", devPath, want)
	}

	fake.assertCallCount(3)
	// First call: zfs get … volsize
	fake.assertArgsContain(0, "zfs", "get", "volsize", "tank/k8s/pvc-new")
	// Second call: zfs create -V <bytes> <dataset>
	fake.assertArgsContain(1, "zfs", "create", "-V", "4294967296", "tank/k8s/pvc-new")
	// Third call: read-back
	fake.assertArgsContain(2, "zfs", "get", "volsize", "tank/k8s/pvc-new")
}

func TestCreate_Idempotent_AlreadyExists(t *testing.T) {
	t.Parallel()

	// The volume exists; expect only one zfs get call, no create.
	fake := newFake(t,
		ok("1073741824\n"), // volsize = 1 GiB
	)
	b := zfs.New("tank", "")
	zfs.SetBackendExec(t, b, fake.exec())

	devPath, allocated, err := b.Create(context.Background(), "tank/pvc-existing", 1<<30, nil)

	if err != nil {
		t.Fatalf("Create (idempotent): unexpected error: %v", err)
	}
	if allocated != 1<<30 {
		t.Errorf("Create (idempotent): allocated = %d; want %d", allocated, 1<<30)
	}
	want := "/dev/zvol/tank/pvc-existing"
	if devPath != want {
		t.Errorf("Create (idempotent): devicePath = %q; want %q", devPath, want)
	}
	fake.assertCallCount(1)
}

func TestCreate_WithProperties(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		notExistResp("pool/pvc-props"), // existence check
		ok(""),                         // create
		ok("2147483648\n"),             // read-back 2 GiB
	)
	b := zfs.New("pool", "")
	zfs.SetBackendExec(t, b, fake.exec())

	params := &agentv1.ZfsVolumeParams{
		Properties: map[string]string{
			"compression": "lz4",
		},
	}
	_, _, err := b.Create(context.Background(), "pool/pvc-props", 2<<30, params)
	if err != nil {
		t.Fatalf("Create with properties: %v", err)
	}

	fake.assertCallCount(3)
	// The create call should include the -o compression=lz4 flag.
	fake.assertArgsContain(1, "zfs", "create", "-V", "-o", "compression=lz4", "pool/pvc-props")
}

func TestCreate_CreateCommandFails(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		notExistResp("tank/pvc-fail"),
		fail("cannot create 'tank/pvc-fail': out of space"),
	)
	b := zfs.New("tank", "")
	zfs.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Create(context.Background(), "tank/pvc-fail", 1<<30, nil)
	if err == nil {
		t.Fatal("Create: expected error when zfs create fails")
	}
	if !strings.Contains(err.Error(), "out of space") {
		t.Errorf("Create: error %q should mention 'out of space'", err)
	}
}

func TestCreate_NoParentDataset(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		notExistResp("mypool/vol1"),
		ok(""),
		ok("536870912\n"), // 512 MiB
	)
	b := zfs.New("mypool", "")
	zfs.SetBackendExec(t, b, fake.exec())

	devPath, allocated, err := b.Create(context.Background(), "mypool/vol1", 512<<20, nil)
	if err != nil {
		t.Fatalf("Create (no parent): %v", err)
	}
	if allocated != 512<<20 {
		t.Errorf("allocated = %d; want %d", allocated, 512<<20)
	}
	if devPath != "/dev/zvol/mypool/vol1" {
		t.Errorf("devicePath = %q; want /dev/zvol/mypool/vol1", devPath)
	}
}

// Delete tests.

func TestDelete_ExistingVolume(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok(""), // zfs destroy succeeds silently
	)
	b := zfs.New("tank", "k8s")
	zfs.SetBackendExec(t, b, fake.exec())

	if err := b.Delete(context.Background(), "tank/pvc-del"); err != nil {
		t.Fatalf("Delete: unexpected error: %v", err)
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "zfs", "destroy", "tank/k8s/pvc-del")
}

func TestDelete_Idempotent_NotExist(t *testing.T) {
	t.Parallel()

	// zfs destroy returns "does not exist" → should be swallowed.
	fake := newFake(t,
		notExistResp("tank/k8s/pvc-gone"),
	)
	b := zfs.New("tank", "k8s")
	zfs.SetBackendExec(t, b, fake.exec())

	if err := b.Delete(context.Background(), "tank/pvc-gone"); err != nil {
		t.Fatalf("Delete (idempotent): expected nil, got: %v", err)
	}
	fake.assertCallCount(1)
}

func TestDelete_OtherError(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		fail("cannot destroy 'tank/k8s/pvc-busy': dataset is busy"),
	)
	b := zfs.New("tank", "k8s")
	zfs.SetBackendExec(t, b, fake.exec())

	err := b.Delete(context.Background(), "tank/pvc-busy")
	if err == nil {
		t.Fatal("Delete: expected error for busy dataset")
	}
	if !strings.Contains(err.Error(), "dataset is busy") {
		t.Errorf("Delete: error %q should mention 'dataset is busy'", err)
	}
}

func TestDelete_NoParentDataset(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok(""),
	)
	b := zfs.New("mypool", "")
	zfs.SetBackendExec(t, b, fake.exec())

	if err := b.Delete(context.Background(), "mypool/vol1"); err != nil {
		t.Fatalf("Delete (no parent): %v", err)
	}
	fake.assertArgsContain(0, "zfs", "destroy", "mypool/vol1")
}

// Expand tests.

func TestExpand_Success(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok(""),             // zfs set volsize=… succeeds
		ok("8589934592\n"), // read-back: 8 GiB
	)
	b := zfs.New("tank", "k8s")
	zfs.SetBackendExec(t, b, fake.exec())

	allocated, err := b.Expand(context.Background(), "tank/pvc-grow", 8<<30)
	if err != nil {
		t.Fatalf("Expand: unexpected error: %v", err)
	}
	if allocated != 8<<30 {
		t.Errorf("Expand: allocated = %d; want %d", allocated, 8<<30)
	}

	fake.assertCallCount(2)
	fake.assertArgsContain(0, "zfs", "set", "volsize=8589934592", "tank/k8s/pvc-grow")
	fake.assertArgsContain(1, "zfs", "get", "volsize", "tank/k8s/pvc-grow")
}

func TestExpand_RoundedUp(t *testing.T) {
	t.Parallel()

	// Request 3 GiB; ZFS rounds to 4 GiB (next volblocksize boundary).
	const requested = 3 << 30
	const rounded = 4 << 30

	fake := newFake(t,
		ok(""),
		ok(fmt.Sprintf("%d\n", rounded)),
	)
	b := zfs.New("data", "")
	zfs.SetBackendExec(t, b, fake.exec())

	allocated, err := b.Expand(context.Background(), "data/pvc-round", requested)
	if err != nil {
		t.Fatalf("Expand (rounded): %v", err)
	}
	if allocated != rounded {
		t.Errorf("Expand: allocated = %d; want %d (rounded)", allocated, rounded)
	}
}

func TestExpand_SetCommandFails(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		fail("cannot set property for 'tank/k8s/pvc-shrink': 'volsize' cannot be decreased"),
	)
	b := zfs.New("tank", "k8s")
	zfs.SetBackendExec(t, b, fake.exec())

	_, err := b.Expand(context.Background(), "tank/pvc-shrink", 1<<20)
	if err == nil {
		t.Fatal("Expand: expected error when zfs set fails")
	}
	if !strings.Contains(err.Error(), "cannot be decreased") {
		t.Errorf("Expand: error %q should mention 'cannot be decreased'", err)
	}
}

func TestExpand_NoParentDataset(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok(""),
		ok("2147483648\n"),
	)
	b := zfs.New("mypool", "")
	zfs.SetBackendExec(t, b, fake.exec())

	allocated, err := b.Expand(context.Background(), "mypool/vol1", 2<<30)
	if err != nil {
		t.Fatalf("Expand (no parent): %v", err)
	}
	if allocated != 2<<30 {
		t.Errorf("Expand: allocated = %d; want %d", allocated, 2<<30)
	}
	fake.assertArgsContain(0, "zfs", "set", "volsize=2147483648", "mypool/vol1")
}

// Capacity tests.

func TestCapacity_Success(t *testing.T) {
	t.Parallel()

	// zpool list -Hp -o size,free tank → "107374182400\t53687091200\n"
	// total = 100 GiB, free = 50 GiB
	const total = int64(100 << 30)
	const free = int64(50 << 30)

	fake := newFake(t,
		ok(fmt.Sprintf("%d\t%d\n", total, free)),
	)
	b := zfs.New("tank", "")
	zfs.SetBackendExec(t, b, fake.exec())

	gotTotal, gotFree, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: unexpected error: %v", err)
	}
	if gotTotal != total {
		t.Errorf("Capacity: totalBytes = %d; want %d", gotTotal, total)
	}
	if gotFree != free {
		t.Errorf("Capacity: freeBytes = %d; want %d", gotFree, free)
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "zpool", "list", "-Hp", "-o", "size,free", "tank")
}

func TestCapacity_PoolNotFound(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		fail("cannot open 'missing-pool': no such pool"),
	)
	b := zfs.New("missing-pool", "")
	zfs.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity: expected error for missing pool")
	}
	if !strings.Contains(err.Error(), "no such pool") {
		t.Errorf("Capacity: error %q should mention 'no such pool'", err)
	}
}

func TestCapacity_MalformedOutput(t *testing.T) {
	t.Parallel()

	// Output with only one column (missing tab separator).
	fake := newFake(t,
		ok("107374182400\n"),
	)
	b := zfs.New("tank", "")
	zfs.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity: expected error for malformed output")
	}
}

func TestCapacity_NonNumericOutput(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok("100G\t50G\n"),
	)
	b := zfs.New("tank", "")
	zfs.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity: expected error for non-numeric output")
	}
}

func TestCapacity_UsesPoolName(t *testing.T) {
	t.Parallel()

	fake := newFake(t,
		ok("21474836480\t10737418240\n"),
	)
	// Use a non-default pool name to verify it is forwarded.
	b := zfs.New("hot-data", "k8s")
	zfs.SetBackendExec(t, b, fake.exec())

	_, _, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: unexpected error: %v", err)
	}

	fake.assertArgsContain(0, "zpool", "list", "hot-data")
}

// ListVolumes tests.

func TestListVolumes_MultipleVolumes(t *testing.T) {
	t.Parallel()

	// Two zvols: pvc-a (4 GiB) and pvc-b (2 GiB), with a parentDataset "k8s".
	output := "tank/k8s/pvc-a\t4294967296\ntank/k8s/pvc-b\t2147483648\n"
	fake := newFake(t, ok(output))
	b := zfs.New("tank", "k8s")
	zfs.SetBackendExec(t, b, fake.exec())

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes: unexpected error: %v", err)
	}
	if len(vols) != 2 {
		t.Fatalf("ListVolumes: got %d volumes; want 2", len(vols))
	}

	// Verify volume IDs are reconstructed without the parentDataset component.
	if vols[0].GetVolumeId() != "tank/pvc-a" {
		t.Errorf("vols[0].VolumeId = %q; want %q", vols[0].GetVolumeId(), "tank/pvc-a")
	}
	if vols[0].GetCapacityBytes() != 4<<30 {
		t.Errorf("vols[0].CapacityBytes = %d; want %d", vols[0].GetCapacityBytes(), int64(4<<30))
	}
	if vols[0].GetDevicePath() != "/dev/zvol/tank/k8s/pvc-a" {
		t.Errorf("vols[0].DevicePath = %q; want %q", vols[0].GetDevicePath(), "/dev/zvol/tank/k8s/pvc-a")
	}

	if vols[1].GetVolumeId() != "tank/pvc-b" {
		t.Errorf("vols[1].VolumeId = %q; want %q", vols[1].GetVolumeId(), "tank/pvc-b")
	}
	if vols[1].GetCapacityBytes() != 2<<30 {
		t.Errorf("vols[1].CapacityBytes = %d; want %d", vols[1].GetCapacityBytes(), int64(2<<30))
	}
	if vols[1].GetDevicePath() != "/dev/zvol/tank/k8s/pvc-b" {
		t.Errorf("vols[1].DevicePath = %q; want %q", vols[1].GetDevicePath(), "/dev/zvol/tank/k8s/pvc-b")
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "zfs", "list", "-Hp", "-t", "volume", "-o", "name,volsize", "-r", "tank/k8s")
}

func TestListVolumes_NoParentDataset(t *testing.T) {
	t.Parallel()

	// Single zvol directly under the pool root (no parentDataset).
	output := "mypool/vol1\t1073741824\n"
	fake := newFake(t, ok(output))
	b := zfs.New("mypool", "")
	zfs.SetBackendExec(t, b, fake.exec())

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes (no parent): unexpected error: %v", err)
	}
	if len(vols) != 1 {
		t.Fatalf("ListVolumes (no parent): got %d volumes; want 1", len(vols))
	}
	if vols[0].GetVolumeId() != "mypool/vol1" {
		t.Errorf("VolumeId = %q; want %q", vols[0].GetVolumeId(), "mypool/vol1")
	}
	if vols[0].GetCapacityBytes() != 1<<30 {
		t.Errorf("CapacityBytes = %d; want %d", vols[0].GetCapacityBytes(), int64(1<<30))
	}
	if vols[0].GetDevicePath() != "/dev/zvol/mypool/vol1" {
		t.Errorf("DevicePath = %q; want %q", vols[0].GetDevicePath(), "/dev/zvol/mypool/vol1")
	}

	fake.assertCallCount(1)
	fake.assertArgsContain(0, "zfs", "list", "-Hp", "-t", "volume", "-o", "name,volsize", "-r", "mypool")
}

func TestListVolumes_EmptyPool(t *testing.T) {
	t.Parallel()

	// zfs list returns success with empty output — no volumes present.
	fake := newFake(t, ok(""))
	b := zfs.New("tank", "k8s")
	zfs.SetBackendExec(t, b, fake.exec())

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes (empty): unexpected error: %v", err)
	}
	if len(vols) != 0 {
		t.Errorf("ListVolumes (empty): got %d volumes; want 0", len(vols))
	}

	fake.assertCallCount(1)
}

func TestListVolumes_DatasetNotExist(t *testing.T) {
	t.Parallel()

	// zfs list returns "does not exist" — treated as empty list, not error.
	fake := newFake(t,
		fail("cannot open 'tank/k8s': dataset does not exist"),
	)
	b := zfs.New("tank", "k8s")
	zfs.SetBackendExec(t, b, fake.exec())

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

	// A generic non-"does not exist" error should be propagated.
	fake := newFake(t,
		fail("permission denied"),
	)
	b := zfs.New("tank", "k8s")
	zfs.SetBackendExec(t, b, fake.exec())

	_, err := b.ListVolumes(context.Background())
	if err == nil {
		t.Fatal("ListVolumes (error): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("ListVolumes (error): error %q should mention 'permission denied'", err)
	}
}

func TestListVolumes_SingleVolume_NoTrailingNewline(t *testing.T) {
	t.Parallel()

	// Output without trailing newline — still should parse correctly.
	output := "pool/k8s/pvc-123\t536870912"
	fake := newFake(t, ok(output))
	b := zfs.New("pool", "k8s")
	zfs.SetBackendExec(t, b, fake.exec())

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes (no newline): unexpected error: %v", err)
	}
	if len(vols) != 1 {
		t.Fatalf("ListVolumes (no newline): got %d volumes; want 1", len(vols))
	}
	if vols[0].GetVolumeId() != "pool/pvc-123" {
		t.Errorf("VolumeId = %q; want %q", vols[0].GetVolumeId(), "pool/pvc-123")
	}
	if vols[0].GetCapacityBytes() != 512<<20 {
		t.Errorf("CapacityBytes = %d; want %d", vols[0].GetCapacityBytes(), int64(512<<20))
	}
}
