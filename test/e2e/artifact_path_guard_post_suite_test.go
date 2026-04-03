package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCWDArtifactsCreatedDuringRunEmpty verifies that an empty directory
// produces no violations.
func TestCWDArtifactsCreatedDuringRunEmpty(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	baseline := time.Now()

	violations := cwdArtifactsCreatedDuringRun(root, baseline)
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations for empty dir, got %d: %v", len(violations), violations)
	}
}

// TestCWDArtifactsCreatedDuringRunPreExisting verifies that files created
// before the baseline are not reported as violations.
func TestCWDArtifactsCreatedDuringRunPreExisting(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// Create a file and then capture the baseline so that the file's mtime
	// is guaranteed to be ≤ baseline.
	oldFile := filepath.Join(root, "pre-existing.txt")
	if err := os.WriteFile(oldFile, []byte("old"), 0o644); err != nil {
		t.Fatalf("write pre-existing file: %v", err)
	}

	// Ensure at least 1 ms has passed so the baseline is strictly after the
	// file's mtime on filesystems with 1-ms mtime resolution.
	time.Sleep(2 * time.Millisecond)
	baseline := time.Now()

	violations := cwdArtifactsCreatedDuringRun(root, baseline)
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations for pre-existing file, got %d: %v", len(violations), violations)
	}
}

// TestCWDArtifactsCreatedDuringRunNewFile verifies that a file created after
// the baseline is reported as a violation.
func TestCWDArtifactsCreatedDuringRunNewFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// Capture baseline before creating the new file.
	time.Sleep(2 * time.Millisecond)
	baseline := time.Now()
	time.Sleep(2 * time.Millisecond)

	newFile := filepath.Join(root, "new-artifact.log")
	if err := os.WriteFile(newFile, []byte("artifact"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}

	violations := cwdArtifactsCreatedDuringRun(root, baseline)
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %v", len(violations), violations)
	}
	if violations[0] != newFile {
		t.Fatalf("violation path = %q, want %q", violations[0], newFile)
	}
}

// TestCWDArtifactsCreatedDuringRunSkipsGitDir verifies that files under .git/
// are excluded from the scan even when they are newer than the baseline.
func TestCWDArtifactsCreatedDuringRunSkipsGitDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	baseline := time.Now()
	time.Sleep(2 * time.Millisecond)

	// Create a file under a .git/ subtree — should be excluded.
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	gitFile := filepath.Join(gitDir, "index")
	if err := os.WriteFile(gitFile, []byte("git"), 0o644); err != nil {
		t.Fatalf("write .git/index: %v", err)
	}

	violations := cwdArtifactsCreatedDuringRun(root, baseline)
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations (.git excluded), got %d: %v", len(violations), violations)
	}
}

// TestCWDArtifactsCreatedDuringRunMixedFiles verifies that only post-baseline
// files are reported when both old and new files exist.
func TestCWDArtifactsCreatedDuringRunMixedFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// Pre-existing file.
	oldFile := filepath.Join(root, "old.txt")
	if err := os.WriteFile(oldFile, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old file: %v", err)
	}

	time.Sleep(2 * time.Millisecond)
	baseline := time.Now()
	time.Sleep(2 * time.Millisecond)

	// New file created after baseline.
	newFile := filepath.Join(root, "subdir", "artifact.out")
	if err := os.MkdirAll(filepath.Dir(newFile), 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	if err := os.WriteFile(newFile, []byte("new"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}

	violations := cwdArtifactsCreatedDuringRun(root, baseline)
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %v", len(violations), violations)
	}
	if violations[0] != newFile {
		t.Fatalf("violation = %q, want %q", violations[0], newFile)
	}
}

// TestCWDArtifactsCreatedDuringRunEmptyRoot verifies that an empty root path
// returns nil without panicking.
func TestCWDArtifactsCreatedDuringRunEmptyRoot(t *testing.T) {
	t.Parallel()

	violations := cwdArtifactsCreatedDuringRun("", time.Now())
	if violations != nil {
		t.Fatalf("expected nil for empty root, got %v", violations)
	}
}

// TestCWDArtifactsCreatedDuringRunDirectoriesNotReported verifies that
// newly-created directories (not files) are not reported as violations —
// only files count.
func TestCWDArtifactsCreatedDuringRunDirectoriesNotReported(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	baseline := time.Now()
	time.Sleep(2 * time.Millisecond)

	// Create a new subdirectory (no files inside it).
	newDir := filepath.Join(root, "newsubdir")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatalf("mkdir newsubdir: %v", err)
	}

	violations := cwdArtifactsCreatedDuringRun(root, baseline)
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations (dirs not counted), got %d: %v", len(violations), violations)
	}
}
