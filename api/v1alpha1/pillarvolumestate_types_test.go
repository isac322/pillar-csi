package v1alpha1

import (
	"math"
	"testing"
)

func TestFilesystemAdoptionInodeConversion(t *testing.T) {
	t.Parallel()

	const maxInode = "18446744073709551615"
	for _, value := range []string{"1", maxInode} {
		got, err := ParseFilesystemAdoptionInode(value)
		if err != nil {
			t.Fatalf("ParseFilesystemAdoptionInode(%q): %v", value, err)
		}
		if got == 0 || FormatFilesystemAdoptionInode(got) != value {
			t.Fatalf("inode conversion = %d/%q, want %q", got, FormatFilesystemAdoptionInode(got), value)
		}
	}

	if got := FormatFilesystemAdoptionInode(math.MaxUint64); got != maxInode {
		t.Fatalf("FormatFilesystemAdoptionInode(MaxUint64) = %q, want %q", got, maxInode)
	}
	if got := FormatFilesystemAdoptionInode(0); got != "" {
		t.Fatalf("FormatFilesystemAdoptionInode(0) = %q, want omitted value", got)
	}

	for _, value := range []string{"", "0", "01", "18446744073709551616", "not-a-number"} {
		if _, err := ParseFilesystemAdoptionInode(value); err == nil {
			t.Errorf("ParseFilesystemAdoptionInode(%q) succeeded, want rejection", value)
		}
	}
}
