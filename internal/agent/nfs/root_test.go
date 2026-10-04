//go:build linux

package nfs

import (
	"context"
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
		slices.Contains(flags, "crossmnt") ||
		slices.Contains(flags, "nohide") {
		t.Fatalf("unsafe pseudoroot policy %s", options)
	}
}

func TestLegacyNestedChildKeepsHiddenMountPolicy(t *testing.T) {
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

func TestAdoptedChildNohidePreservesRootAndLegacyACLs(t *testing.T) {
	t.Parallel()
	r := &memoryRuntime{}
	m := testManager(t, r, t.TempDir())
	m.config.BeforeActivate = func(context.Context, Export) error { return nil }
	legacy, adopted := testExport(t, m), testExport(t, m)
	legacy.Clients = []string{"192.0.2.20"}
	adopted.VolumeID = "pool/adopted"
	adopted.Clients = []string{"192.0.2.21"}
	adopted.SourceKey = "filesystem/native-key"
	adopted.FenceUID = "uid-1"
	for _, e := range []Export{legacy, adopted} {
		if err := m.Put(t.Context(), e, true, true); err != nil {
			t.Fatal(err)
		}
	}
	assertPolicies := func(adoptedActive bool) {
		t.Helper()
		wanted := map[[2]string]bool{
			{m.config.ExportRoot, legacy.Clients[0]}: true,
			{legacy.Path, legacy.Clients[0]}:         true,
		}
		if adoptedActive {
			wanted[[2]string{m.config.ExportRoot, adopted.Clients[0]}] = true
			wanted[[2]string{adopted.Path, adopted.Clients[0]}] = true
		}
		for _, row := range r.rows {
			key := [2]string{row.Path, row.Client}
			if !wanted[key] {
				t.Fatalf("unexpected child or pseudoroot admission: %#v", row)
			}
			delete(wanted, key)
			flags := strings.Split(row.Options, ",")
			if row.Path == m.config.ExportRoot {
				assertRootPolicy(t, row.Options)
			} else if slices.Contains(flags, "crossmnt") ||
				slices.Contains(flags, "nohide") != (row.Path == adopted.Path) {
				t.Fatalf("wrong child traversal policy: %#v", row)
			}
		}
		if len(wanted) != 0 {
			t.Fatalf("missing child or pseudoroot admissions: %#v", wanted)
		}
	}
	assertPolicies(true)

	// Repair both pre-nohide adopted rows and the unsafe root crossmnt policy.
	for i := range r.rows {
		switch r.rows[i].Path {
		case adopted.Path:
			r.rows[i].Options = strings.TrimSuffix(r.rows[i].Options, ",nohide") + ",hide,nocrossmnt"
		case legacy.Path:
			r.rows[i].Options += ",nohide"
		case m.config.ExportRoot:
			r.rows[i].Options += ",crossmnt"
		}
	}
	if err := m.Put(t.Context(), adopted, true, true); err != nil {
		t.Fatal(err)
	}
	assertPolicies(true)
	if err := m.ChangeClient(t.Context(), adopted.VolumeID, adopted.Clients[0], false); err != nil {
		t.Fatal(err)
	}
	assertPolicies(false)
	if err := m.ChangeClient(t.Context(), adopted.VolumeID, adopted.Clients[0], true); err != nil {
		t.Fatal(err)
	}
	assertPolicies(true)
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
	rootWanted := "ro,sync,no_subtree_check,secure,sec=sys,fsid=0,root_squash"
	for _, actual := range []string{
		"rw,sync,no_subtree_check,secure,sec=sys,fsid=0,root_squash",
		rootWanted + ",no_root_squash",
		rootWanted + ",insecure",
		rootWanted + ",crossmnt",
		rootWanted + ",nohide",
		strings.Replace(rootWanted, "fsid=0", "fsid=123", 1),
	} {
		if optionsMatch(actual, rootWanted) {
			t.Fatalf("weaker or foreign admission accepted: %q", actual)
		}
	}
	legacyWanted := strings.Replace(rootWanted, "fsid=0", "fsid="+FSID("pool/legacy"), 1)
	for _, wanted := range []string{rootWanted, legacyWanted} {
		if !optionsMatch(wanted+",wdelay,hide,nocrossmnt", wanted) {
			t.Fatal("security-equivalent nfs-utils defaults rejected")
		}
		for _, actual := range []string{wanted + ",crossmnt", wanted + ",nohide"} {
			if optionsMatch(actual, wanted) {
				t.Fatalf("mount traversal accepted on root or legacy export: %q", actual)
			}
		}
	}
	adoptedWanted := strings.Replace(legacyWanted, FSID("pool/legacy"), FSID("pool/adopted"), 1) + ",nohide"
	for _, actual := range []string{
		strings.TrimSuffix(adoptedWanted, ",nohide"),
		adoptedWanted + ",hide",
		adoptedWanted + ",crossmnt",
		adoptedWanted + ",no_root_squash",
		adoptedWanted + ",insecure",
		strings.Replace(adoptedWanted, FSID("pool/adopted"), FSID("pool/foreign"), 1),
	} {
		if optionsMatch(actual, adoptedWanted) {
			t.Fatalf("weaker or foreign adopted admission accepted: %q", actual)
		}
	}
	if !optionsMatch(adoptedWanted+",wdelay,nocrossmnt", adoptedWanted) {
		t.Fatal("adopted nohide admission with nfs-utils defaults rejected")
	}
}
