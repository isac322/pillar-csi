//go:build linux

package directory

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/isac322/pillar-csi/internal/agent/backend"
)

const walkProject uint32 = 4242

type nativeProject struct {
	id      uint32
	inherit bool
}

// nativeProjects models the kernel's per-inode project attributes for a real
// temporary tree. The walker reads it through its project-attribute reader;
// the quota inode count is what the kernel would charge to walkProject.
type nativeProjects map[[2]uint64]nativeProject

func (n nativeProjects) set(t *testing.T, path string, id uint32) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	n[[2]uint64{st.Dev, st.Ino}] = nativeProject{id: id, inherit: st.Mode&unix.S_IFMT == unix.S_IFDIR}
}

func (n nativeProjects) read(fd int) (projectID uint32, inherit bool, err error) {
	var st unix.Stat_t
	err = unix.Fstat(fd, &st)
	if err != nil {
		return 0, false, err
	}
	attrs, ok := n[[2]uint64{st.Dev, st.Ino}]
	if !ok {
		return 0, false, unix.ENOTTY
	}
	return attrs.id, attrs.inherit, nil
}

func (n nativeProjects) quotaInodes(project uint32) uint64 {
	var count uint64
	for _, attrs := range n {
		if attrs.id == project {
			count++
		}
	}
	return count
}

type treeEntry struct {
	mode   uint32
	ino    uint64
	nlink  uint64
	size   int64
	mtime  unix.Timespec
	ctime  unix.Timespec
	target string
}

func snapshotTree(t *testing.T, root string) map[string]treeEntry {
	t.Helper()
	tree := make(map[string]treeEntry)
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		var st unix.Stat_t
		statErr := unix.Lstat(path, &st)
		if statErr != nil {
			return statErr
		}
		entry := treeEntry{
			mode:  st.Mode,
			ino:   st.Ino,
			nlink: normalizeLinkCount(st.Nlink),
			size:  st.Size,
			mtime: st.Mtim,
			ctime: st.Ctim,
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			target, linkErr := os.Readlink(path)
			if linkErr != nil {
				return linkErr
			}
			entry.target = target
		}
		tree[path] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func mkdirProject(t *testing.T, n nativeProjects, path string, id uint32) {
	t.Helper()
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	n.set(t, path, id)
}

func writeProject(t *testing.T, n nativeProjects, path string, id uint32) {
	t.Helper()
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	n.set(t, path, id)
}

func symlinkProject(t *testing.T, n nativeProjects, target, path string, id uint32) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	n.set(t, path, id)
}

// hardlink links the entry itself, including a symlink, without following it.
func hardlink(t *testing.T, existing, path string) {
	t.Helper()
	if err := unix.Linkat(unix.AT_FDCWD, existing, unix.AT_FDCWD, path, 0); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyProjectTreeAdmitsOnlyVerifiedProjectScope(t *testing.T) {
	tests := []struct {
		name  string
		build func(t *testing.T, n nativeProjects, src, out string)
		admit bool
	}{
		{
			name: "uncharged symlink cannot compensate for an external project member",
			build: func(t *testing.T, n nativeProjects, src, out string) {
				symlinkProject(t, n, "target", filepath.Join(src, "link"), 0)
				writeProject(t, n, filepath.Join(out, "member"), walkProject)
			},
		},
		{
			name: "external project member without symlinks",
			build: func(t *testing.T, n nativeProjects, src, out string) {
				writeProject(t, n, filepath.Join(src, "file"), walkProject)
				writeProject(t, n, filepath.Join(out, "member"), walkProject)
			},
		},
		{
			name: "provably uncharged symlink is admitted",
			build: func(t *testing.T, n nativeProjects, src, _ string) {
				writeProject(t, n, filepath.Join(src, "file"), walkProject)
				symlinkProject(t, n, "file", filepath.Join(src, "link"), 0)
			},
			admit: true,
		},
		{
			name: "charged symlink membership is unprovable",
			build: func(t *testing.T, n nativeProjects, src, _ string) {
				symlinkProject(t, n, "target", filepath.Join(src, "link"), walkProject)
			},
		},
		{
			name: "regular hard links inside source count one inode",
			build: func(t *testing.T, n nativeProjects, src, _ string) {
				mkdirProject(t, n, filepath.Join(src, "sub"), walkProject)
				writeProject(t, n, filepath.Join(src, "a"), walkProject)
				hardlink(t, filepath.Join(src, "a"), filepath.Join(src, "sub", "b"))
			},
			admit: true,
		},
		{
			name: "uncharged symlink hard links inside source are admitted",
			build: func(t *testing.T, n nativeProjects, src, _ string) {
				symlinkProject(t, n, "target", filepath.Join(src, "l1"), 0)
				hardlink(t, filepath.Join(src, "l1"), filepath.Join(src, "l2"))
			},
			admit: true,
		},
		{
			name: "uncharged symlink hard link outside source is refused",
			build: func(t *testing.T, n nativeProjects, src, out string) {
				symlinkProject(t, n, "target", filepath.Join(src, "link"), 0)
				hardlink(t, filepath.Join(src, "link"), filepath.Join(out, "link"))
			},
		},
		{
			name: "charged symlink hard link outside source is refused",
			build: func(t *testing.T, n nativeProjects, src, out string) {
				symlinkProject(t, n, "target", filepath.Join(src, "link"), walkProject)
				hardlink(t, filepath.Join(src, "link"), filepath.Join(out, "link"))
			},
		},
		{
			name: "regular hard link outside source is refused",
			build: func(t *testing.T, n nativeProjects, src, out string) {
				writeProject(t, n, filepath.Join(src, "a"), walkProject)
				hardlink(t, filepath.Join(src, "a"), filepath.Join(out, "a"))
			},
		},
		{
			name: "regular file with another project is refused",
			build: func(t *testing.T, n nativeProjects, src, _ string) {
				writeProject(t, n, filepath.Join(src, "file"), walkProject+1)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := runProjectWalk(t, tc.build)
			if tc.admit {
				if err != nil {
					t.Fatalf("verifyProjectTree refused an exclusively owned source: %v", err)
				}
				return
			}
			var refused *backend.ImportRefusedError
			if !errors.As(err, &refused) || refused.Reason != reasonQuota {
				t.Fatalf("verifyProjectTree = %v, want quota refusal", err)
			}
		})
	}
}

// runProjectWalk builds a real source tree and an outside sibling, runs the
// walker with the modeled kernel project state, and fails if the walk changed
// either tree. It returns the walker's admission result.
func runProjectWalk(t *testing.T, build func(t *testing.T, n nativeProjects, src, out string)) error {
	t.Helper()
	base := t.TempDir()
	src := filepath.Join(base, "src")
	out := filepath.Join(base, "out")
	native := make(nativeProjects)
	mkdirProject(t, native, src, walkProject)
	mkdirProject(t, native, out, 0)
	build(t, native, src, out)

	before := snapshotTree(t, base)
	fd, err := unix.Open(src, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = verifyProjectTree(
		context.Background(),
		fd,
		walkProject,
		native.quotaInodes(walkProject),
		native.read,
	)
	if closeErr := unix.Close(fd); closeErr != nil {
		t.Fatal(closeErr)
	}
	if after := snapshotTree(t, base); !reflect.DeepEqual(before, after) {
		t.Fatalf("project walk changed the source tree:\nbefore=%v\nafter=%v", before, after)
	}
	return err
}
