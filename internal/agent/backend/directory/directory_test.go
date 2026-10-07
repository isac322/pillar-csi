//go:build linux

package directory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isac322/pillar-csi/internal/agent/backend"
)

func TestQuotaConversionsRejectOverflow(t *testing.T) {
	got, err := checkedQuotaBytes(4, 10)
	if err != nil || got != 4096 {
		t.Fatalf("checkedQuotaBytes(4,10) = %d, %v", got, err)
	}
	if _, err := checkedQuotaBytes((uint64(1<<63-1)>>9)+1, 9); err == nil {
		t.Fatal("expected quota conversion overflow")
	}
	if got, err := checkedFSBytes(0, 4096); err != nil || got != 0 {
		t.Fatalf("checkedFSBytes(0,4096) = %d, %v; zero available space is valid", got, err)
	}
	if _, err := checkedFSBytes(uint64(1<<63-1), 2); err == nil {
		t.Fatal("expected statfs conversion overflow")
	}
}

func TestReadOnlyVolumeOperationsPreserveSource(t *testing.T) {
	root := t.TempDir()
	b := New("pool", root)
	if _, _, err := b.Create(context.Background(), "vol", 1, nil); err == nil {
		t.Fatal("Create unexpectedly succeeded")
	}
	if err := b.Delete(context.Background(), "vol"); err == nil {
		t.Fatal("Delete unexpectedly succeeded")
	}
	if _, err := b.Expand(context.Background(), "vol", 1); err == nil {
		t.Fatal("Expand unexpectedly succeeded")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("read-only operations created %d entries", len(entries))
	}
}

func TestInspectRejectsOutsideAndLayoutAlias(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	b := New("pool", root)
	if _, err := b.InspectImport(context.Background(), outside, 1, nil, backend.Layout{HostRoot: root}); err == nil {
		t.Fatal("outside source unexpectedly accepted")
	}
	if _, err := b.InspectImport(
		context.Background(),
		root,
		1,
		nil,
		backend.Layout{HostRoot: filepath.Join(root, "alias")},
	); err == nil {
		t.Fatal("layout alias unexpectedly accepted")
	}
}

func TestFormatUUIDCanonicalLowercase(t *testing.T) {
	got := formatUUID([]byte{
		0xAB, 0xCD, 0xEF, 0x01, 0x23, 0x45, 0x67, 0x89,
		0x10, 0x32, 0x54, 0x76, 0x98, 0xBA, 0xDC, 0xFE,
	})
	if got != "abcdef01-2345-6789-1032-547698badcfe" {
		t.Fatalf("formatUUID = %q", got)
	}
	if strings.ToLower(got) != got {
		t.Fatal("UUID is not lowercase")
	}
}
