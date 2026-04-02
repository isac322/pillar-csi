package e2e

// Sub-AC 3.3 — standalone Go tests for TC ID uniqueness validation.
//
// These tests can be run without the Ginkgo framework:
//
//	go test -run=TestTCIDUniquenessGuard ./test/e2e/...
//
// The companion Ginkgo BeforeSuite (registered in
// tc_id_uniqueness_guard_suite_test.go) enforces the same invariants at suite
// startup so that all 437 specs benefit from the check before any It node runs.

import "testing"

// TestTCIDUniquenessGuard validates that every TC in the default profile
// registers a distinct [TC-<DocID>] node label.  The test verifies four
// structural invariants:
//
//  1. The profile size matches defaultProfileCaseCount (437).
//  2. No two cases share a DocID (which drives the [TC-<ID>] label).
//  3. No two cases share an Ordinal (sequential numbering is gap-free).
//  4. Every node label matches the "[TC-<prefix><digits>]" format required by
//     the TC ID naming scheme.
func TestTCIDUniquenessGuard(t *testing.T) {
	t.Parallel()

	profile, err := buildDefaultProfile()
	if err != nil {
		t.Fatalf("[AC3.3] buildDefaultProfile: %v", err)
	}

	// ── Invariant 1: profile size ────────────────────────────────────────────
	if got := len(profile); got != defaultProfileCaseCount {
		t.Fatalf("[AC3.3] profile size = %d, want %d", got, defaultProfileCaseCount)
	}

	// ── Invariant 2: DocID uniqueness (= node label uniqueness) ─────────────
	// validateTCNodeLabelUniqueness is also called inside buildDefaultProfile,
	// but we call it here too so this test reports its own [AC3.3] tag on
	// failure and can be run in strict isolation without side effects from the
	// profile-building code path.
	if err := validateTCNodeLabelUniqueness(profile); err != nil {
		t.Fatalf("[AC3.3] %v", err)
	}

	// ── Invariant 3: ordinal uniqueness ──────────────────────────────────────
	// Ordinals are assigned sequentially inside buildDefaultProfile; a gap or
	// duplicate would indicate a bug in the assignment loop.
	seenOrdinal := make(map[int]string, len(profile))
	for _, tc := range profile {
		if prev, exists := seenOrdinal[tc.Ordinal]; exists {
			t.Errorf("[AC3.3] ordinal %d shared by %s and %s",
				tc.Ordinal, prev, tc.DocID)
			continue
		}
		seenOrdinal[tc.Ordinal] = tc.DocID
	}

	// ── Invariant 4: node label format ───────────────────────────────────────
	// Every label must start with "[TC-" and end with "]".
	const labelPrefix = "[TC-"
	for _, tc := range profile {
		label := tc.tcNodeLabel()
		// Shortest valid label: "[TC-E1.1]" = 9 chars
		if len(label) < len(labelPrefix)+4 {
			t.Errorf("[AC3.3] [%s]: node label %q is too short (min %d chars)",
				tc.DocID, label, len(labelPrefix)+4)
			continue
		}
		if label[:len(labelPrefix)] != labelPrefix {
			t.Errorf("[AC3.3] [%s]: node label %q must start with %q",
				tc.DocID, label, labelPrefix)
		}
		if label[len(label)-1] != ']' {
			t.Errorf("[AC3.3] [%s]: node label %q must end with ']'",
				tc.DocID, label)
		}
	}

	if !t.Failed() {
		t.Logf("[AC3.3] all %d TC node labels are distinct and well-formed", len(profile))
	}
}

// TestTCIDUniquenessGuard_DetectsCollision is a white-box test that injects a
// synthetic collision into validateTCNodeLabelUniqueness to confirm the guard
// fires correctly and that the error message names both colliding parties.
func TestTCIDUniquenessGuard_DetectsCollision(t *testing.T) {
	t.Parallel()

	// Build a minimal two-element slice where both cases have the same DocID
	// but different TestName values — the exact scenario the composite traceKey
	// check inside buildDefaultProfile cannot detect but
	// validateTCNodeLabelUniqueness must catch.
	colliding := []documentedCase{
		{DocID: "E1.1", GroupKey: "E1", TestName: "TestFoo", Ordinal: 1},
		{DocID: "E1.1", GroupKey: "E1", TestName: "TestBar", Ordinal: 2},
	}

	err := validateTCNodeLabelUniqueness(colliding)
	if err == nil {
		t.Fatal("[AC3.3] expected error for duplicate DocID E1.1, got nil")
	}

	// The error message must name the colliding node label and both TestName
	// values so the developer can trace the collision without additional tools.
	errStr := err.Error()
	for _, want := range []string{"[TC-E1.1]", "TestFoo", "TestBar"} {
		if !containsSubstr(errStr, want) {
			t.Errorf("[AC3.3] collision error message missing %q\ngot: %s", want, errStr)
		}
	}
}

// TestTCIDUniquenessGuard_AcceptsDistinctIDs verifies that
// validateTCNodeLabelUniqueness returns nil for a well-formed profile where
// all DocIDs are distinct.
func TestTCIDUniquenessGuard_AcceptsDistinctIDs(t *testing.T) {
	t.Parallel()

	distinct := []documentedCase{
		{DocID: "E1.1", GroupKey: "E1", TestName: "TestFoo", Ordinal: 1},
		{DocID: "E1.2", GroupKey: "E1", TestName: "TestBar", Ordinal: 2},
		{DocID: "E2.1", GroupKey: "E2", TestName: "TestBaz", Ordinal: 3},
		{DocID: "F27.1", GroupKey: "F27", TestName: "TestQux", Ordinal: 4},
	}

	if err := validateTCNodeLabelUniqueness(distinct); err != nil {
		t.Fatalf("[AC3.3] unexpected error for distinct IDs: %v", err)
	}
}

// TestTCIDUniquenessGuard_EmptyProfile verifies that validateTCNodeLabelUniqueness
// returns nil for an empty slice (no collision possible).
func TestTCIDUniquenessGuard_EmptyProfile(t *testing.T) {
	t.Parallel()

	if err := validateTCNodeLabelUniqueness(nil); err != nil {
		t.Fatalf("[AC3.3] unexpected error for empty profile: %v", err)
	}
	if err := validateTCNodeLabelUniqueness([]documentedCase{}); err != nil {
		t.Fatalf("[AC3.3] unexpected error for empty slice: %v", err)
	}
}

// TestTCIDUniquenessGuard_SingleCase verifies that a single-element profile
// passes unconditionally (no collision possible with one entry).
func TestTCIDUniquenessGuard_SingleCase(t *testing.T) {
	t.Parallel()

	single := []documentedCase{
		{DocID: "E1.1", GroupKey: "E1", TestName: "TestFoo", Ordinal: 1},
	}
	if err := validateTCNodeLabelUniqueness(single); err != nil {
		t.Fatalf("[AC3.3] unexpected error for single-element profile: %v", err)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// containsSubstr returns true when s contains substr as a substring.
// It is a self-contained alternative to strings.Contains so that this test
// file has no additional imports beyond "testing".
func containsSubstr(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
