package names_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/names"
)

// dnsLabelRe matches a valid Kubernetes DNS-label: starts and ends with an
// alphanumeric character; interior may contain hyphens; total ≤ 63 chars.
var dnsLabelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)

func isValidDNSLabel(s string) bool {
	if len(s) < 2 || len(s) > 63 {
		return false
	}
	return dnsLabelRe.MatchString(s)
}

// --- Namespace ---

func TestNamespace_ValidDNSLabel(t *testing.T) {
	tcIDs := []string{"E1.1", "F2.3", "M4.5", "E10.100"}
	for _, id := range tcIDs {
		ns := names.Namespace(id)
		if !isValidDNSLabel(ns) {
			t.Errorf("Namespace(%q) = %q: not a valid DNS label", id, ns)
		}
	}
}

func TestNamespace_Length(t *testing.T) {
	tcIDs := []string{"E1.1", "F2.3", "M4.5", "E10.100"}
	for _, id := range tcIDs {
		ns := names.Namespace(id)
		if len(ns) > 63 {
			t.Errorf("Namespace(%q) = %q: length %d > 63", id, ns, len(ns))
		}
	}
}

func TestNamespace_Uniqueness(t *testing.T) {
	tcIDs := []string{
		"E1.1", "E1.2", "E1.3",
		"F2.3", "F2.4",
		"M4.5", "M4.6",
		"E10.100", "E10.101",
	}
	seen := make(map[string]string)
	for _, id := range tcIDs {
		ns := names.Namespace(id)
		if prev, ok := seen[ns]; ok {
			t.Errorf("Namespace collision: %q and %q both produce %q", prev, id, ns)
		}
		seen[ns] = id
	}
}

func TestNamespace_Determinism(t *testing.T) {
	for _, id := range []string{"E1.1", "F2.3", "M4.5", "E10.100"} {
		a := names.Namespace(id)
		b := names.Namespace(id)
		if a != b {
			t.Errorf("Namespace(%q) not deterministic: got %q then %q", id, a, b)
		}
	}
}

func TestNamespace_HasPrefix(t *testing.T) {
	for _, id := range []string{"E1.1", "F2.3", "M4.5"} {
		ns := names.Namespace(id)
		if !strings.HasPrefix(ns, "e2e-tc-") {
			t.Errorf("Namespace(%q) = %q: expected prefix e2e-tc-", id, ns)
		}
	}
}

// --- ObjectPrefix ---

func TestObjectPrefix_ValidDNSLabel(t *testing.T) {
	for _, id := range []string{"E1.1", "F2.3", "M4.5", "E10.100"} {
		p := names.ObjectPrefix(id)
		if !isValidDNSLabel(p) {
			t.Errorf("ObjectPrefix(%q) = %q: not a valid DNS label", id, p)
		}
	}
}

func TestObjectPrefix_Uniqueness(t *testing.T) {
	tcIDs := []string{
		"E1.1", "E1.2", "F2.3", "M4.5", "E10.100",
	}
	seen := make(map[string]string)
	for _, id := range tcIDs {
		p := names.ObjectPrefix(id)
		if prev, ok := seen[p]; ok {
			t.Errorf("ObjectPrefix collision: %q and %q both produce %q", prev, id, p)
		}
		seen[p] = id
	}
}

func TestObjectPrefix_Determinism(t *testing.T) {
	for _, id := range []string{"E1.1", "F2.3", "M4.5"} {
		a := names.ObjectPrefix(id)
		b := names.ObjectPrefix(id)
		if a != b {
			t.Errorf("ObjectPrefix(%q) not deterministic: got %q then %q", id, a, b)
		}
	}
}

// --- ResourceName ---

func TestResourceName_LengthLimit(t *testing.T) {
	cases := []struct {
		tcID   string
		suffix string
	}{
		{"E1.1", "pv"},
		{"E1.1", "pvc"},
		{"E1.1", "a-very-long-suffix-that-might-push-things-over-the-limit-abcdef"},
		{"E10.100", "storageclass"},
	}
	for _, c := range cases {
		rn := names.ResourceName(c.tcID, c.suffix)
		if len(rn) > 63 {
			t.Errorf("ResourceName(%q, %q) = %q: length %d > 63", c.tcID, c.suffix, rn, len(rn))
		}
	}
}

func TestResourceName_NoTrailingHyphen(t *testing.T) {
	cases := []struct {
		tcID   string
		suffix string
	}{
		{"E1.1", "pv"},
		{"E10.100", "a-very-long-suffix-that-might-push-things-over-the-limit-abcdef"},
	}
	for _, c := range cases {
		rn := names.ResourceName(c.tcID, c.suffix)
		if strings.HasSuffix(rn, "-") {
			t.Errorf("ResourceName(%q, %q) = %q: has trailing hyphen", c.tcID, c.suffix, rn)
		}
	}
}

func TestResourceName_ContainsPrefix(t *testing.T) {
	for _, id := range []string{"E1.1", "F2.3"} {
		prefix := names.ObjectPrefix(id)
		rn := names.ResourceName(id, "pvc")
		if !strings.HasPrefix(rn, prefix) {
			t.Errorf("ResourceName(%q, pvc) = %q: does not start with ObjectPrefix %q", id, rn, prefix)
		}
	}
}

// --- Cross-function Uniqueness ---

// Namespace and ObjectPrefix for the same TC ID must be different strings
// (they serve different resource types).
func TestNamespaceAndPrefix_AreDifferent(t *testing.T) {
	for _, id := range []string{"E1.1", "F2.3", "M4.5"} {
		ns := names.Namespace(id)
		p := names.ObjectPrefix(id)
		if ns == p {
			t.Errorf("Namespace and ObjectPrefix for %q are identical: %q", id, ns)
		}
	}
}

// Two different TC IDs must produce different namespaces AND different prefixes.
func TestDifferentTCIDs_ProduceDifferentNames(t *testing.T) {
	pairs := [][2]string{
		{"E1.1", "E1.2"},
		{"F2.3", "F2.4"},
		{"M4.5", "M4.6"},
		// Normalization collision candidate: dots vs underscores map to hyphens,
		// but the hash distinguishes them.
		{"E1-1", "E1.1"},
	}
	for _, pair := range pairs {
		ns1 := names.Namespace(pair[0])
		ns2 := names.Namespace(pair[1])
		if ns1 == ns2 {
			t.Errorf("Namespace(%q)==Namespace(%q)==%q: should differ", pair[0], pair[1], ns1)
		}
		p1 := names.ObjectPrefix(pair[0])
		p2 := names.ObjectPrefix(pair[1])
		if p1 == p2 {
			t.Errorf("ObjectPrefix(%q)==ObjectPrefix(%q)==%q: should differ", pair[0], pair[1], p1)
		}
	}
}
