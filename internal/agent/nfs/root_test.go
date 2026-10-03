//go:build linux

package nfs

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func rootAdmission(r *memoryRuntime, root string) []string {
	var clients []string
	for _, row := range r.rows {
		if row.Path == root {
			clients = append(clients, row.Client)
		}
	}
	slices.Sort(clients)
	return clients
}

func TestPseudorootUnionDoesNotWidenChildACL(t *testing.T) {
	t.Parallel()
	r := &memoryRuntime{}
	m := testManager(t, r, t.TempDir())
	first, second := testExport(t, m), testExport(t, m)
	second.VolumeID = "pool/second"
	first.Clients = []string{"192.0.2.20"}
	second.Clients = []string{"192.0.2.21"}
	for _, e := range []Export{first, second} {
		if err := m.Put(t.Context(), e, true, true); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(rootAdmission(r, m.config.ExportRoot), []string{"192.0.2.20", "192.0.2.21"}) {
		t.Fatal("pseudoroot cannot traverse both authorized child datasets")
	}
	assertPrivateChildACLs(t, r, m.config.ExportRoot, first, second)
	public := testExport(t, m)
	public.VolumeID = "pool/public"
	public.ACLEnabled = false
	if err := m.Put(t.Context(), public, true, true); err != nil {
		t.Fatal(err)
	}
	if err := m.ChangeClient(t.Context(), public.VolumeID, "192.0.2.20", false); err != nil {
		t.Fatalf("ACL-disabled unpublish failed: %v", err)
	}
	if !reflect.DeepEqual(rootAdmission(r, m.config.ExportRoot), []string{"*"}) {
		t.Fatal("ACL-disabled child cannot be traversed")
	}
	assertPrivateChildACLs(t, r, m.config.ExportRoot, first, second)
	if err := m.Remove(t.Context(), public.VolumeID); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rootAdmission(r, m.config.ExportRoot), []string{"192.0.2.20", "192.0.2.21"}) {
		t.Fatal("public sibling removal left wildcard root")
	}
	if err := m.ChangeClient(t.Context(), first.VolumeID, "192.0.2.20", false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rootAdmission(r, m.config.ExportRoot), []string{"192.0.2.21"}) {
		t.Fatal("peer revoke removed surviving client's pseudoroot traversal")
	}
	if err := m.Remove(t.Context(), second.VolumeID); err != nil {
		t.Fatal(err)
	}
	if len(r.rows) != 0 {
		t.Fatal("no admitted child but reachable root remains")
	}
}

func assertPrivateChildACLs(t *testing.T, r *memoryRuntime, root string, first, second Export) {
	t.Helper()
	for _, row := range r.rows {
		if row.Path == root {
			assertRootPolicy(t, row.Options)
		}
		if row.Path == first.Path && row.Client != "192.0.2.20" {
			t.Fatal("sibling volume client gained first volume")
		}
		if row.Path == second.Path && row.Client != "192.0.2.21" {
			t.Fatal("sibling volume client gained second volume")
		}
	}
}

func assertRootPolicy(t *testing.T, options string) {
	t.Helper()
	flags := strings.Split(options, ",")
	if !strings.HasPrefix(options, "ro,") ||
		!slices.Contains(flags, optionRootSquash) ||
		!slices.Contains(flags, "crossmnt") ||
		slices.Contains(flags, "nohide") {
		t.Fatalf("unsafe pseudoroot policy %s", options)
	}
}

func TestOwnedNestedChildUsesCrossmntPseudoroot(t *testing.T) {
	t.Parallel()
	r := &memoryRuntime{}
	m := testManager(t, r, t.TempDir())
	e := testExport(t, m)
	e.VolumeID = "pool/nested"
	e.Clients = []string{"192.0.2.20"}
	e.Path = filepath.Join(m.config.ExportRoot, "pool", "nested", "volume")
	if err := os.MkdirAll(e.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(filepath.Join(m.config.ExportRoot, "pool")); err != nil {
			t.Error(err)
		}
	})
	if err := m.Put(t.Context(), e, true, true); err != nil {
		t.Fatal(err)
	}
	var rootSeen, childSeen bool
	for _, row := range r.rows {
		switch row.Path {
		case m.config.ExportRoot:
			rootSeen = true
			assertRootPolicy(t, row.Options)
		case e.Path:
			childSeen = true
			flags := strings.Split(row.Options, ",")
			if slices.Contains(flags, "crossmnt") || slices.Contains(flags, "nohide") {
				t.Fatalf("nested child weakened pseudoroot policy: %s", row.Options)
			}
			if row.Client != e.Clients[0] {
				t.Fatalf("nested child admitted unexpected client %q", row.Client)
			}
		default:
			t.Fatalf("unexpected foreign admission for nested child: %#v", row)
		}
	}
	if !rootSeen || !childSeen {
		t.Fatalf("nested owned child admission incomplete: root=%v child=%v", rootSeen, childSeen)
	}
}

func TestClientRoutingStaysWithinOwnedPseudoroot(t *testing.T) {
	t.Parallel()
	r := &memoryRuntime{}
	m := testManager(t, r, t.TempDir())
	path, err := m.ExportPath(m.config.ExportRoot + "/pool/pillar/volume")
	if err != nil || path != "/pool/pillar/volume" {
		t.Fatalf("client routing=%q error=%v", path, err)
	}
	for _, physical := range []string{m.config.ExportRoot, m.config.ExportRoot + "/../outside", "/foreign/volume"} {
		if _, err := m.ExportPath(physical); err == nil {
			t.Fatalf("foreign physical path %q routed through pseudoroot", physical)
		}
	}
}

func TestExportPolicyReadbackRejectsWeakerKernelAdmission(t *testing.T) {
	t.Parallel()
	rootWanted := "ro,sync,no_subtree_check,secure,sec=sys,fsid=0,root_squash,crossmnt"
	for _, actual := range []string{
		"rw,sync,no_subtree_check,secure,sec=sys,fsid=0,root_squash,crossmnt",
		rootWanted + ",no_root_squash",
		rootWanted + ",insecure",
		strings.TrimSuffix(rootWanted, ",crossmnt"),
		rootWanted + ",nocrossmnt",
		rootWanted + ",nohide",
		strings.Replace(rootWanted, "fsid=0", "fsid=123", 1),
	} {
		if optionsMatch(actual, rootWanted) {
			t.Fatalf("weaker or foreign admission accepted: %q", actual)
		}
	}
	childWanted := strings.TrimSuffix(rootWanted, ",crossmnt")
	if optionsMatch(childWanted+",crossmnt", childWanted) {
		t.Fatal("crossmnt accepted on a child export")
	}
	if !optionsMatch(childWanted+",wdelay,hide,nocrossmnt", childWanted) {
		t.Fatal("security-equivalent nfs-utils defaults rejected")
	}
}
