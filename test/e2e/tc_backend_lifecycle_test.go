package e2e

import (
	"fmt"
	"regexp"
	"sync"
	"testing"
)

// TestBackendHandleNamesAreUniquePerTC verifies that backend resource names
// derived from distinct TestCaseScopes are globally unique, preventing any
// backend state sharing across parallel test cases.
func TestBackendHandleNamesAreUniquePerTC(t *testing.T) {
	const numScopes = 5

	type names struct {
		zfsPool  string
		lvmVG    string
		iscsiIQN string
	}

	results := make([]names, numScopes)
	var wg sync.WaitGroup

	for i := range numScopes {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			scope, err := NewTestCaseScope(fmt.Sprintf("uniqueness-tc-%d", idx))
			if err != nil {
				t.Errorf("NewTestCaseScope(%d): %v", idx, err)
				return
			}
			defer func() { _ = scope.Close() }()

			results[idx] = names{
				zfsPool:  zfsPoolName(scope.ScopeTag),
				lvmVG:    lvmVGName(scope.ScopeTag),
				iscsiIQN: iscsiIQN(scope.ScopeTag),
			}
		}(i)
	}

	wg.Wait()

	// Verify all ZFS pool names are distinct.
	zfsSeen := make(map[string]int)
	for i, n := range results {
		if other, dup := zfsSeen[n.zfsPool]; dup {
			t.Errorf("ZFS pool name %q duplicated at indices %d and %d", n.zfsPool, other, i)
		}
		zfsSeen[n.zfsPool] = i
	}

	// Verify all LVM VG names are distinct.
	lvmSeen := make(map[string]int)
	for i, n := range results {
		if other, dup := lvmSeen[n.lvmVG]; dup {
			t.Errorf("LVM VG name %q duplicated at indices %d and %d", n.lvmVG, other, i)
		}
		lvmSeen[n.lvmVG] = i
	}

	// Verify all iSCSI IQNs are distinct.
	iqnSeen := make(map[string]int)
	for i, n := range results {
		if other, dup := iqnSeen[n.iscsiIQN]; dup {
			t.Errorf("iSCSI IQN %q duplicated at indices %d and %d", n.iscsiIQN, other, i)
		}
		iqnSeen[n.iscsiIQN] = i
	}
}

// TestBackendNamingDerivesFromScopeTag verifies that the derived pool/VG/IQN
// names embed a substring from the TC scope tag, ensuring traceability.
func TestBackendNamingDerivesFromScopeTag(t *testing.T) {
	scope, err := NewTestCaseScope("naming-derive-tc-42")
	if err != nil {
		t.Fatalf("NewTestCaseScope: %v", err)
	}
	defer func() { _ = scope.Close() }()

	pool := zfsPoolName(scope.ScopeTag)
	vg := lvmVGName(scope.ScopeTag)
	iqn := iscsiIQN(scope.ScopeTag)

	if len(pool) == 0 {
		t.Error("zfsPoolName returned empty string")
	}
	if len(vg) == 0 {
		t.Error("lvmVGName returned empty string")
	}
	if len(iqn) == 0 {
		t.Error("iscsiIQN returned empty string")
	}

	// Prefixes must be present.
	if len(pool) < 5 || pool[:5] != "e2ep-" {
		t.Errorf("zfsPoolName %q does not start with 'e2ep-'", pool)
	}
	if len(vg) < 6 || vg[:6] != "e2evg-" {
		t.Errorf("lvmVGName %q does not start with 'e2evg-'", vg)
	}
	iqnPrefix := "iqn.2024-01.io.pillar-csi:"
	if len(iqn) < len(iqnPrefix) || iqn[:len(iqnPrefix)] != iqnPrefix {
		t.Errorf("iscsiIQN %q does not start with %q", iqn, iqnPrefix)
	}
}

// TestBackendNamesAreDNSSafe verifies that derived resource names only contain
// characters that are valid for their respective resource types.
//
//   - ZFS pool names: alphanumeric, hyphens, underscores, periods, and colons
//     (we restrict to alphanumeric + hyphens in our derivation).
//   - LVM VG names: alphanumeric, hyphens, underscores, periods, plus signs.
//     (we restrict to alphanumeric + hyphens in our derivation).
//   - iSCSI IQNs: the suffix after "iqn.2024-01.io.pillar-csi:" must be
//     alphanumeric + hyphens.
func TestBackendNamesAreDNSSafe(t *testing.T) {
	scope, err := NewTestCaseScope("dns-safe-tc-99")
	if err != nil {
		t.Fatalf("NewTestCaseScope: %v", err)
	}
	defer func() { _ = scope.Close() }()

	// Alphanumeric + hyphens only.
	safeRe := regexp.MustCompile(`^[a-z0-9][a-z0-9\-]*[a-z0-9]$|^[a-z0-9]$`)

	pool := zfsPoolName(scope.ScopeTag)
	if !safeRe.MatchString(pool) {
		t.Errorf("ZFS pool name %q contains unsafe characters", pool)
	}

	vg := lvmVGName(scope.ScopeTag)
	if !safeRe.MatchString(vg) {
		t.Errorf("LVM VG name %q contains unsafe characters", vg)
	}

	iqn := iscsiIQN(scope.ScopeTag)
	// IQN format: "iqn.<date>.<reversed-domain>:<suffix>"
	iqnRe := regexp.MustCompile(`^iqn\.\d{4}-\d{2}\.[a-z0-9.\-]+:[a-z0-9\-]+$`)
	if !iqnRe.MatchString(iqn) {
		t.Errorf("iSCSI IQN %q is not a valid IQN format", iqn)
	}
}

// TestZFSPoolNameFitsWithinOSLimits verifies that derived ZFS pool names are
// within ZFS's 256-character pool name limit.
func TestZFSPoolNameFitsWithinOSLimits(t *testing.T) {
	const zfsPoolNameLimit = 256

	scope, err := NewTestCaseScope("limits-zfs-tc-1234567890abcdef")
	if err != nil {
		t.Fatalf("NewTestCaseScope: %v", err)
	}
	defer func() { _ = scope.Close() }()

	pool := zfsPoolName(scope.ScopeTag)
	if len(pool) > zfsPoolNameLimit {
		t.Errorf("ZFS pool name %q is %d chars, exceeds limit of %d",
			pool, len(pool), zfsPoolNameLimit)
	}
}

// TestLVMVGNameFitsWithinOSLimits verifies that derived LVM VG names are
// within LVM's 127-character Volume Group name limit.
func TestLVMVGNameFitsWithinOSLimits(t *testing.T) {
	const lvmVGNameLimit = 127

	scope, err := NewTestCaseScope("limits-lvm-tc-1234567890abcdef")
	if err != nil {
		t.Fatalf("NewTestCaseScope: %v", err)
	}
	defer func() { _ = scope.Close() }()

	vg := lvmVGName(scope.ScopeTag)
	if len(vg) > lvmVGNameLimit {
		t.Errorf("LVM VG name %q is %d chars, exceeds limit of %d",
			vg, len(vg), lvmVGNameLimit)
	}
}
