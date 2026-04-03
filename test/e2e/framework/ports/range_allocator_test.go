package ports_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/ports"
)

// ─── ISCSIPortRange.Port ─────────────────────────────────────────────────────

func TestISCSIPortRange_Port_ReturnsBaseForIndexZero(t *testing.T) {
	r := &ports.ISCSIPortRange{CaseIndex: 0, Base: 30100, Count: 10, End: 30110}
	if got := r.Port(0); got != 30100 {
		t.Errorf("Port(0) = %d, want 30100", got)
	}
}

func TestISCSIPortRange_Port_ReturnsLastInRange(t *testing.T) {
	r := &ports.ISCSIPortRange{CaseIndex: 0, Base: 30100, Count: 10, End: 30110}
	if got := r.Port(9); got != 30109 {
		t.Errorf("Port(9) = %d, want 30109", got)
	}
}

func TestISCSIPortRange_Port_PanicsOnNegativeIndex(t *testing.T) {
	r := &ports.ISCSIPortRange{CaseIndex: 0, Base: 30100, Count: 10, End: 30110}
	defer func() {
		if recover() == nil {
			t.Error("expected panic for negative index, but none occurred")
		}
	}()
	_ = r.Port(-1)
}

func TestISCSIPortRange_Port_PanicsOnIndexEqualToCount(t *testing.T) {
	r := &ports.ISCSIPortRange{CaseIndex: 0, Base: 30100, Count: 10, End: 30110}
	defer func() {
		if recover() == nil {
			t.Error("expected panic for index == Count, but none occurred")
		}
	}()
	_ = r.Port(10)
}

func TestISCSIPortRange_Port_PanicsOnNilReceiver(t *testing.T) {
	var r *ports.ISCSIPortRange
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil receiver, but none occurred")
		}
	}()
	_ = r.Port(0)
}

// ─── ISCSIPortRange.Contains ─────────────────────────────────────────────────

func TestISCSIPortRange_Contains_TrueForPortsInsideRange(t *testing.T) {
	r := &ports.ISCSIPortRange{Base: 30100, Count: 10, End: 30110}
	for p := 30100; p < 30110; p++ {
		if !r.Contains(p) {
			t.Errorf("Contains(%d) = false, want true", p)
		}
	}
}

func TestISCSIPortRange_Contains_FalseForPortsOutsideRange(t *testing.T) {
	r := &ports.ISCSIPortRange{Base: 30100, Count: 10, End: 30110}
	outside := []int{30099, 30110, 30111, 1, 65535}
	for _, p := range outside {
		if r.Contains(p) {
			t.Errorf("Contains(%d) = true, want false", p)
		}
	}
}

func TestISCSIPortRange_Contains_NilRangeReturnsFalse(t *testing.T) {
	var r *ports.ISCSIPortRange
	if r.Contains(30100) {
		t.Error("nil.Contains(30100) = true, want false")
	}
}

// ─── ISCSIPortRange.Overlaps ─────────────────────────────────────────────────

func TestISCSIPortRange_Overlaps_AdjacentRangesDoNotOverlap(t *testing.T) {
	a := &ports.ISCSIPortRange{Base: 30100, Count: 10, End: 30110}
	b := &ports.ISCSIPortRange{Base: 30110, Count: 10, End: 30120}
	if a.Overlaps(b) {
		t.Error("adjacent ranges [30100,30110) and [30110,30120) overlap, want false")
	}
	if b.Overlaps(a) {
		t.Error("adjacent ranges [30110,30120) and [30100,30110) overlap, want false")
	}
}

func TestISCSIPortRange_Overlaps_OverlappingRangesReturnTrue(t *testing.T) {
	a := &ports.ISCSIPortRange{Base: 30100, Count: 15, End: 30115}
	b := &ports.ISCSIPortRange{Base: 30110, Count: 10, End: 30120}
	if !a.Overlaps(b) {
		t.Error("overlapping ranges should return true")
	}
	if !b.Overlaps(a) {
		t.Error("overlapping ranges should return true (commutative)")
	}
}

func TestISCSIPortRange_Overlaps_IdenticalRangesOverlap(t *testing.T) {
	a := &ports.ISCSIPortRange{Base: 30100, Count: 10, End: 30110}
	b := &ports.ISCSIPortRange{Base: 30100, Count: 10, End: 30110}
	if !a.Overlaps(b) {
		t.Error("identical ranges must overlap")
	}
}

func TestISCSIPortRange_Overlaps_NilReceiverReturnsFalse(t *testing.T) {
	var a *ports.ISCSIPortRange
	b := &ports.ISCSIPortRange{Base: 30100, Count: 10, End: 30110}
	if a.Overlaps(b) {
		t.Error("nil.Overlaps(non-nil) = true, want false")
	}
}

func TestISCSIPortRange_Overlaps_NilArgumentReturnsFalse(t *testing.T) {
	a := &ports.ISCSIPortRange{Base: 30100, Count: 10, End: 30110}
	if a.Overlaps(nil) {
		t.Error("non-nil.Overlaps(nil) = true, want false")
	}
}

// ─── ISCSIPortRange.String ───────────────────────────────────────────────────

func TestISCSIPortRange_String_ContainsCaseIndexAndPorts(t *testing.T) {
	r := &ports.ISCSIPortRange{CaseIndex: 3, Base: 30130, Count: 10, End: 30140}
	s := r.String()
	for _, want := range []string{"3", "30130", "30139"} {
		found := false
		for i := 0; i+len(want) <= len(s); i++ {
			if s[i:i+len(want)] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("String() = %q, want substring %q", s, want)
		}
	}
}

func TestISCSIPortRange_String_NilReturnsNonEmpty(t *testing.T) {
	var r *ports.ISCSIPortRange
	if s := r.String(); s == "" {
		t.Error("nil.String() returned empty string")
	}
}

// ─── NewISCSIRangeAllocator validation ───────────────────────────────────────

func TestNewISCSIRangeAllocator_PanicsOnPortBelow1024(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for basePort < 1024, but none occurred")
		}
	}()
	_ = ports.NewISCSIRangeAllocator(1023, 10)
}

func TestNewISCSIRangeAllocator_PanicsOnZeroPortsPerCase(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for portsPerCase < 1, but none occurred")
		}
	}()
	_ = ports.NewISCSIRangeAllocator(30100, 0)
}

func TestNewISCSIRangeAllocator_PanicsWhenRangeExceeds65535(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for basePort + portsPerCase > 65535, but none occurred")
		}
	}()
	_ = ports.NewISCSIRangeAllocator(65530, 10)
}

func TestNewISCSIRangeAllocator_AcceptsValidParameters(t *testing.T) {
	a := ports.NewISCSIRangeAllocator(30100, 10)
	if a == nil {
		t.Fatal("NewISCSIRangeAllocator returned nil")
	}
}

// ─── ISCSIRangeAllocator.Allocate ────────────────────────────────────────────

func TestISCSIRangeAllocator_Allocate_FirstRangeStartsAtBase(t *testing.T) {
	a := ports.NewISCSIRangeAllocator(30200, 5)
	r := a.Allocate()

	if r.CaseIndex != 0 {
		t.Errorf("CaseIndex = %d, want 0", r.CaseIndex)
	}
	if r.Base != 30200 {
		t.Errorf("Base = %d, want 30200", r.Base)
	}
	if r.Count != 5 {
		t.Errorf("Count = %d, want 5", r.Count)
	}
	if r.End != 30205 {
		t.Errorf("End = %d, want 30205", r.End)
	}
}

func TestISCSIRangeAllocator_Allocate_SequentialRangesAreContiguous(t *testing.T) {
	a := ports.NewISCSIRangeAllocator(30300, 10)
	r0 := a.Allocate()
	r1 := a.Allocate()
	r2 := a.Allocate()

	if r0.End != r1.Base {
		t.Errorf("gap between case 0 and case 1: r0.End=%d, r1.Base=%d", r0.End, r1.Base)
	}
	if r1.End != r2.Base {
		t.Errorf("gap between case 1 and case 2: r1.End=%d, r2.Base=%d", r1.End, r2.Base)
	}
}

func TestISCSIRangeAllocator_Allocate_IndexIsMonotonicallyIncreasing(t *testing.T) {
	const n = 10
	a := ports.NewISCSIRangeAllocator(30400, 5)
	prev := -1
	for i := range n {
		r := a.Allocate()
		if r.CaseIndex != prev+1 {
			t.Errorf("allocation %d: CaseIndex = %d, want %d", i, r.CaseIndex, prev+1)
		}
		prev = r.CaseIndex
	}
}

func TestISCSIRangeAllocator_Allocate_RangesNeverOverlap(t *testing.T) {
	a := ports.NewISCSIRangeAllocator(30500, 10)
	const n = 50
	ranges := make([]*ports.ISCSIPortRange, n)
	for i := range n {
		ranges[i] = a.Allocate()
	}

	for i := range n {
		for j := i + 1; j < n; j++ {
			if ranges[i].Overlaps(ranges[j]) {
				t.Errorf("range[%d]=%s overlaps range[%d]=%s",
					i, ranges[i], j, ranges[j])
			}
		}
	}
}

func TestISCSIRangeAllocator_Allocate_BaseComputedCorrectly(t *testing.T) {
	const base = 30600
	const stride = 7
	a := ports.NewISCSIRangeAllocator(base, stride)

	for i := range 20 {
		r := a.Allocate()
		wantBase := base + i*stride
		if r.Base != wantBase {
			t.Errorf("case %d: Base = %d, want %d", i, r.Base, wantBase)
		}
		if r.End != wantBase+stride {
			t.Errorf("case %d: End = %d, want %d", i, r.End, wantBase+stride)
		}
	}
}

// ─── Concurrent allocation ────────────────────────────────────────────────────

func TestISCSIRangeAllocator_Allocate_ConcurrentCallsProduceNoOverlap(t *testing.T) {
	const goroutines = 50
	a := ports.NewISCSIRangeAllocator(30700, 10)

	type result struct {
		r   *ports.ISCSIPortRange
		err error
	}
	results := make(chan result, goroutines)

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := a.Allocate()
			if r == nil {
				results <- result{err: fmt.Errorf("goroutine %d: Allocate returned nil", i)}
				return
			}
			results <- result{r: r}
		}(i)
	}
	wg.Wait()
	close(results)

	var collected []*ports.ISCSIPortRange
	for res := range results {
		if res.err != nil {
			t.Errorf("concurrent allocation error: %v", res.err)
			continue
		}
		collected = append(collected, res.r)
	}

	// Every CaseIndex must be unique.
	seenIdx := make(map[int]int)
	for _, r := range collected {
		seenIdx[r.CaseIndex]++
	}
	for idx, count := range seenIdx {
		if count > 1 {
			t.Errorf("CaseIndex %d allocated %d times; expected at most 1", idx, count)
		}
	}

	// No two ranges may overlap.
	for i := range len(collected) {
		for j := i + 1; j < len(collected); j++ {
			if collected[i].Overlaps(collected[j]) {
				t.Errorf("range[%d]=%s overlaps range[%d]=%s",
					i, collected[i], j, collected[j])
			}
		}
	}
}

// ─── TotalAllocated ──────────────────────────────────────────────────────────

func TestISCSIRangeAllocator_TotalAllocated_TracksCount(t *testing.T) {
	a := ports.NewISCSIRangeAllocator(31000, 10)

	if got := a.TotalAllocated(); got != 0 {
		t.Errorf("TotalAllocated before any allocations = %d, want 0", got)
	}
	for i := range 5 {
		_ = a.Allocate()
		if got := a.TotalAllocated(); got != i+1 {
			t.Errorf("TotalAllocated after %d allocations = %d, want %d", i+1, got, i+1)
		}
	}
}

// ─── GlobalISCSIRangeAllocator ────────────────────────────────────────────────

func TestGlobalISCSIRangeAllocator_IsNonNil(t *testing.T) {
	if ports.GlobalISCSIRangeAllocator == nil {
		t.Fatal("ports.GlobalISCSIRangeAllocator is nil")
	}
}

func TestGlobalISCSIRangeAllocator_AllocatesUniqueNonOverlappingRanges(t *testing.T) {
	// Allocate two ranges from the global allocator.  Because other tests (or
	// the TestCaseScope) may also consume from the global allocator, we cannot
	// predict the absolute Base values — but we can verify non-overlap.
	r1 := ports.GlobalISCSIRangeAllocator.Allocate()
	r2 := ports.GlobalISCSIRangeAllocator.Allocate()

	if r1.CaseIndex == r2.CaseIndex {
		t.Errorf("GlobalISCSIRangeAllocator returned same CaseIndex %d twice", r1.CaseIndex)
	}
	if r1.Overlaps(r2) {
		t.Errorf("GlobalISCSIRangeAllocator returned overlapping ranges: %s, %s", r1, r2)
	}
}

func TestGlobalISCSIRangeAllocator_BasePortMatchesConstant(t *testing.T) {
	if ports.GlobalISCSIRangeAllocator.BasePort != ports.ISCSIRangeBasePort {
		t.Errorf("GlobalISCSIRangeAllocator.BasePort = %d, want %d (ISCSIRangeBasePort)",
			ports.GlobalISCSIRangeAllocator.BasePort, ports.ISCSIRangeBasePort)
	}
}

func TestGlobalISCSIRangeAllocator_PortsPerCaseMatchesConstant(t *testing.T) {
	if ports.GlobalISCSIRangeAllocator.PortsPerCase != ports.ISCSIPortsPerCase {
		t.Errorf("GlobalISCSIRangeAllocator.PortsPerCase = %d, want %d (ISCSIPortsPerCase)",
			ports.GlobalISCSIRangeAllocator.PortsPerCase, ports.ISCSIPortsPerCase)
	}
}

// ─── Integration: ranges from ISCSIPortsPerCase=10 cover 421 TCs ─────────────

func TestISCSIRangeAllocator_421TestCasesFitInPortSpace(t *testing.T) {
	a := ports.NewISCSIRangeAllocator(ports.ISCSIRangeBasePort, ports.ISCSIPortsPerCase)
	const numCases = 421

	var last *ports.ISCSIPortRange
	for range numCases {
		last = a.Allocate()
	}

	if last == nil {
		t.Fatal("last Allocate() returned nil")
	}
	if last.End > 65535 {
		t.Errorf("421 test cases exceed port space: last range ends at %d (> 65535)", last.End)
	}
	t.Logf("421 TCs: last range = %s, max port used = %d", last, last.End-1)
}
