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

	params := &agentv1.BackendParams{
		Params: &agentv1.BackendParams_Zfs{
			Zfs: &agentv1.ZfsVolumeParams{
				Properties: map[string]string{
					"compression": "lz4",
				},
			},
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

// capacityFake answers only `zfs get -Hp -o name,property,value <props>
// <datasets...>` for the datasets it knows; every other command fails, so a
// Capacity implementation that consults pool-wide zpool(8) figures cannot
// pass.  Each map value is a property→value map; a dataset absent from the
// map makes the whole command fail, mirroring zfs(8).
func capacityFake(
	datasets map[string]map[string]string,
) func(_ context.Context, name string, args ...string) ([]byte, error) {
	prefix := []string{"get", "-Hp", "-o", "name,property,value"}
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "zfs" || len(args) < len(prefix)+2 ||
			strings.Join(args[:len(prefix)], " ") != strings.Join(prefix, " ") {
			return nil, fmt.Errorf("capacityFake: unexpected command %s %v", name, args)
		}
		var sb strings.Builder
		for _, ds := range args[len(prefix)+1:] {
			p, found := datasets[ds]
			if !found {
				return []byte(fmt.Sprintf("cannot open '%s': dataset does not exist", ds)),
					errors.New("exit status 1")
			}
			for prop := range strings.SplitSeq(args[len(prefix)], ",") {
				v, ok := p[prop]
				if !ok {
					return nil, fmt.Errorf("capacityFake: unmodeled property %q for %s", prop, ds)
				}
				fmt.Fprintf(&sb, "%s\t%s\t%s\n", ds, prop, v)
			}
		}
		return []byte(sb.String()), nil
	}
}

// capProps builds a dataset property map for capacityFake.
func capProps(used, available, usedbyrefreservation, refquota, quota, reservation int64) map[string]string {
	return map[string]string{
		"used":                 fmt.Sprint(used),
		"available":            fmt.Sprint(available),
		"usedbyrefreservation": fmt.Sprint(usedbyrefreservation),
		"refquota":             fmt.Sprint(refquota),
		"quota":                fmt.Sprint(quota),
		"reservation":          fmt.Sprint(reservation),
	}
}

func TestCapacity_ReportsProvisioningRootDataset(t *testing.T) {
	t.Parallel()

	// hot-data/k8s is bounded by a 100 GiB quota (used 8 GiB, available
	// 92 GiB) while the pool root still has far more available.  zvols are
	// created under hot-data/k8s, so its figures are the allocatable ones.
	datasets := map[string]map[string]string{
		"hot-data":     capProps(44<<30, 176<<30, 0, 0, 0, 0),
		"hot-data/k8s": capProps(8<<30, 92<<30, 0, 0, 100<<30, 0),
	}

	tests := []struct {
		name          string
		parentDataset string
		wantTotal     int64
		wantAvailable int64
	}{
		{name: "parent dataset", parentDataset: "k8s", wantTotal: 100 << 30, wantAvailable: 92 << 30},
		{name: "pool root dataset", parentDataset: "", wantTotal: 220 << 30, wantAvailable: 176 << 30},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := zfs.NewWithExecFn("hot-data", tc.parentDataset, capacityFake(datasets))

			total, avail, err := b.Capacity(context.Background())
			if err != nil {
				t.Fatalf("Capacity: unexpected error: %v", err)
			}
			if avail != tc.wantAvailable {
				t.Errorf("Capacity: availableBytes = %d; want %d", avail, tc.wantAvailable)
			}
			if total != tc.wantTotal {
				t.Errorf("Capacity: totalBytes = %d; want used+available %d", total, tc.wantTotal)
			}
		})
	}
}

func TestCapacity_RefquotaParentWithReservation(t *testing.T) {
	t.Parallel()

	// A hierarchical reservation larger than the dataset's used adds its
	// unused slack to the dir-level bound (dsl_dir_space_available:
	// parentspace += reserved - used).  k8s: reservation 300MiB, used
	// 100MiB → +200MiB over the parent bound.
	datasets := map[string]map[string]string{
		"hot-data":     capProps(0, 500<<20, 0, 0, 0, 0),
		"hot-data/k8s": capProps(100<<20, 4<<20, 0, 16<<20, 0, 300<<20),
	}
	b := zfs.NewWithExecFn("hot-data", "k8s", capacityFake(datasets))

	_, avail, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: unexpected error: %v", err)
	}
	// B(hot-data) = 500MiB; k8s has no quota: B(k8s) = 500 + (300-100) = 700MiB.
	if want := int64(700 << 20); avail != want {
		t.Errorf("Capacity: availableBytes = %d; want %d (parent bound + unused reservation)", avail, want)
	}
}

func TestCapacity_SubtractsParentRefreservation(t *testing.T) {
	t.Parallel()

	// The parent's unused refreservation is added to its `available` but is
	// not usable by child zvols (verified on OpenZFS 2.4.1: a create sized
	// available-16MiB fails while available-usedbyrefreservation-16MiB
	// succeeds).
	datasets := map[string]map[string]string{
		"hot-data":     capProps(0, 872<<20, 0, 0, 0, 0),
		"hot-data/k8s": capProps(0, 872<<20, 384<<20, 0, 0, 0),
	}
	b := zfs.NewWithExecFn("hot-data", "k8s", capacityFake(datasets))

	total, avail, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: unexpected error: %v", err)
	}
	wantAvail := int64(872<<20) - int64(384<<20)
	if avail != wantAvail {
		t.Errorf("Capacity: availableBytes = %d; want %d (available - usedbyrefreservation)", avail, wantAvail)
	}
	if total != wantAvail {
		t.Errorf("Capacity: totalBytes = %d; want %d", total, wantAvail)
	}
}

func TestCapacity_RefquotaParentUsesAncestorBound(t *testing.T) {
	t.Parallel()

	// refquota caps the parent's own `available` but does not bound child
	// zvols (verified on OpenZFS 2.4.1: a 256MiB zvol created fine under a
	// parent whose available was ~4MiB).  The bound is recomputed from the
	// ancestor's dir-level space: min(B(parent), quota - used).
	datasets := map[string]map[string]string{
		"hot-data":     capProps(0, 872<<20, 0, 0, 0, 0),
		"hot-data/k8s": capProps(12<<20, 4<<20, 0, 16<<20, 0, 0),
	}
	b := zfs.NewWithExecFn("hot-data", "k8s", capacityFake(datasets))

	_, avail, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: unexpected error: %v", err)
	}
	// hot-data has no refquota: B(hot-data) = 872MiB - 0.  k8s has no quota:
	// B(k8s) = B(hot-data) = 872MiB, not the refquota-clamped 4MiB.
	if want := int64(872 << 20); avail != want {
		t.Errorf("Capacity: availableBytes = %d; want %d (ancestor bound, refquota ignored)", avail, want)
	}
}

func TestCapacity_RefquotaParentUnderQuotaAncestor(t *testing.T) {
	t.Parallel()

	// Mirrors the executed discriminator: q2 quota=200M, q2/rq refquota=16M.
	// Predicted bound = q2.available - q2.usedbyrefreservation = 197071872.
	datasets := map[string]map[string]string{
		"hot-data":       capProps(0, 872<<20, 0, 0, 0, 0),
		"hot-data/q2":    capProps(3<<20, 197<<20, 0, 0, 200<<20, 0),
		"hot-data/q2/rq": capProps(12<<20, 4<<20, 0, 16<<20, 0, 0),
	}
	b := zfs.NewWithExecFn("hot-data", "q2/rq", capacityFake(datasets))

	_, avail, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: unexpected error: %v", err)
	}
	if want := int64(197 << 20); avail != want {
		t.Errorf("Capacity: availableBytes = %d; want %d", avail, want)
	}
}

func TestCapacity_RefquotaParentOverQuota(t *testing.T) {
	t.Parallel()

	// used > quota on the refquota'd parent: nothing is allocatable.
	datasets := map[string]map[string]string{
		"hot-data":     capProps(0, 872<<20, 0, 0, 0, 0),
		"hot-data/k8s": capProps(150<<20, 0, 0, 16<<20, 100<<20, 0),
	}
	b := zfs.NewWithExecFn("hot-data", "k8s", capacityFake(datasets))

	_, avail, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: unexpected error: %v", err)
	}
	if avail != 0 {
		t.Errorf("Capacity: availableBytes = %d; want 0 (used > quota)", avail)
	}
}

func TestCapacity_PoolRootRefquotaFails(t *testing.T) {
	t.Parallel()

	// The pool root's dir-level bound includes the pool slop term, which is
	// not a user-visible property; an explicit error beats a fabricated
	// number.
	datasets := map[string]map[string]string{
		"hot-data": capProps(44<<30, 4<<30, 0, 8<<30, 0, 0),
	}
	b := zfs.NewWithExecFn("hot-data", "", capacityFake(datasets))

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity: expected error for refquota on pool root")
	}
	if !strings.Contains(err.Error(), "hot-data") || !strings.Contains(err.Error(), "refquota") {
		t.Errorf("Capacity: error %q should name the dataset and the refquota property", err)
	}
}

func TestCapacity_MissingProvisioningRootFails(t *testing.T) {
	t.Parallel()

	// The pool exists but the parent dataset does not: Create would fail, so
	// Capacity must not report pool-wide space as allocatable.
	b := zfs.NewWithExecFn("hot-data", "absent", capacityFake(map[string]map[string]string{
		"hot-data": capProps(44<<30, 176<<30, 0, 0, 0, 0),
	}))

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("Capacity: expected error for missing parent dataset")
	}
	if !strings.Contains(err.Error(), "hot-data/absent") || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Capacity: error %q should name the missing dataset and the cause", err)
	}
}

func TestCapacity_InvalidOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
	}{
		{name: "one column", out: "tank\tused\n"},
		{name: "two columns", out: "tank\tused\t1\textra\n"},
		{name: "non-numeric", out: "tank\tused\t100G\n"},
		{name: "negative", out: "tank\tused\t-1\n"},
		{name: "missing used property", out: "tank\tavailable\t1024\n"},
		{name: "missing available property", out: "tank\tused\t1\ntank\trefquota\t0\n"},
		{name: "empty", out: "\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newFake(t, ok(tc.out))
			b := zfs.New("tank", "")
			zfs.SetBackendExec(t, b, fake.exec())

			if _, _, err := b.Capacity(context.Background()); err == nil {
				t.Fatalf("Capacity: expected error for output %q", tc.out)
			}
		})
	}
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
