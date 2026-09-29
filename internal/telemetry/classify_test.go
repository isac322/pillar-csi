package telemetry

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

// Documented label sets (spec metrics M7, M8, M9). Kept literal here so a
// classifier change that emits an undocumented value fails the test.
var (
	documentedExecutables = setOf(
		"zfs", "lvs", "vgs", "lvcreate", "lvremove", "lvextend", "dmsetup",
		"mkfs.ext4", "mkfs.ext3", "mkfs.ext2", "mkfs.xfs", "resize2fs", "xfs_growfs", "other",
	)
	documentedSubcommands = setOf(
		"", "other",
		"create", "destroy", "set", "get", "list",
		"reload", "resume", "suspend", "remove", "table", "info", "status",
	)
	documentedErrnos = setOf(
		"EBUSY", "EINVAL", "ENOENT", "EEXIST", "EPERM", "EACCES", "ENOSPC", "ENODEV", "other",
	)
	documentedFenceDecisions = setOf(
		"admit_new_lifecycle", "admit_advance", "admit_same_generation", "admit_terminal_retry",
		"reject_missing_token", "reject_retired", "reject_other_owner", "reject_ended",
		"reject_superseded", "reject_mark_changed", "mark_io_error", "other",
	)
	documentedFenceOps = setOf("grant", "revoke", "destroy", "other")
)

// T3: every classifier maps unknown input to "other" and only ever emits a
// documented value.
func TestExecLabelClosure(t *testing.T) {
	tests := []struct {
		name       string
		cmd        string
		args       []string
		executable string
		subcommand string
	}{
		{"zfs create", "zfs", []string{"create", "-V", "1G", "tank/pvc-1"}, "zfs", "create"},
		{"zfs by path", "/usr/sbin/zfs", []string{"get", "-H", "-p", "volsize", "tank/pvc-1"}, "zfs", "get"},
		{"zfs flag before subcommand", "zfs", []string{"-H", "list"}, "zfs", "list"},
		{"zfs unknown subcommand", "zfs", []string{"snapshot", "tank/pvc-1@s"}, "zfs", "other"},
		{"zfs without subcommand", "zfs", nil, "zfs", "other"},
		{"dmsetup create after flags", "/sbin/dmsetup", []string{"--noudevsync", "create", "pillar-x"}, "dmsetup", "create"},
		{"dmsetup unknown subcommand", "dmsetup", []string{"--noudevsync", "mknodes"}, "dmsetup", "other"},
		{"lvcreate has no subcommand", "lvcreate", []string{"-L", "1G", "-n", "pvc-1", "vg"}, "lvcreate", ""},
		{"mkfs.xfs", "mkfs.xfs", []string{"/dev/dm-0"}, "mkfs.xfs", ""},
		{"resize2fs by path", "/usr/sbin/resize2fs", []string{"/dev/nvme0n1"}, "resize2fs", ""},
		{"unknown mkfs", "mkfs.btrfs", []string{"/dev/dm-0"}, "other", ""},
		{"unknown executable", "rm", []string{"create"}, "other", ""},
		{"empty name", "", nil, "other", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exe := ExecExecutableLabel(tc.cmd)
			sub := ExecSubcommandLabel(exe, tc.args)
			if exe != tc.executable || sub != tc.subcommand {
				t.Fatalf("labels = (%q, %q), want (%q, %q)", exe, sub, tc.executable, tc.subcommand)
			}
			assertDocumented(t, "executable", documentedExecutables, exe)
			assertDocumented(t, "subcommand", documentedSubcommands, sub)
		})
	}
}

func TestConfigfsErrnoLabelClosure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"path error EBUSY", &os.PathError{Op: "rmdir", Path: "/sys/kernel/config/nvmet/x", Err: syscall.EBUSY}, "EBUSY"},
		{
			"wrapped path error EINVAL",
			fmt.Errorf("write attr: %w", &os.PathError{Op: "write", Path: "p", Err: syscall.EINVAL}),
			"EINVAL",
		},
		{"link error EEXIST", &os.LinkError{Op: "symlink", Old: "a", New: "b", Err: syscall.EEXIST}, "EEXIST"},
		{"ENOENT", &os.PathError{Op: "open", Path: "p", Err: syscall.ENOENT}, "ENOENT"},
		{"EPERM", &os.PathError{Op: "write", Path: "p", Err: syscall.EPERM}, "EPERM"},
		{"EACCES", &os.PathError{Op: "write", Path: "p", Err: syscall.EACCES}, "EACCES"},
		{"ENOSPC", &os.PathError{Op: "write", Path: "p", Err: syscall.ENOSPC}, "ENOSPC"},
		{"ENODEV", &os.PathError{Op: "write", Path: "p", Err: syscall.ENODEV}, "ENODEV"},
		{"undocumented errno", &os.PathError{Op: "write", Path: "p", Err: syscall.EIO}, "other"},
		{"no errno in chain", errors.New("short write"), "other"},
		{"nil", nil, "other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ConfigfsErrnoLabel(tc.err)
			if got != tc.want {
				t.Fatalf("ConfigfsErrnoLabel() = %q, want %q", got, tc.want)
			}
			assertDocumented(t, "errno", documentedErrnos, got)
		})
	}
}

func TestFenceLabelClosure(t *testing.T) {
	for d := range documentedFenceDecisions {
		if got := FenceDecisionLabel(d); got != d {
			t.Errorf("FenceDecisionLabel(%q) = %q, want it unchanged", d, got)
		}
	}
	for _, unknown := range []string{"", "admit", "Reject_Ended", "reject_unknown"} {
		got := FenceDecisionLabel(unknown)
		if got != LabelOther {
			t.Errorf("FenceDecisionLabel(%q) = %q, want %q", unknown, got, LabelOther)
		}
		assertDocumented(t, "decision", documentedFenceDecisions, got)
	}
	for op := range documentedFenceOps {
		if got := FenceOpLabel(op); got != op {
			t.Errorf("FenceOpLabel(%q) = %q, want it unchanged", op, got)
		}
	}
	for _, unknown := range []string{"", "expand", "GRANT"} {
		got := FenceOpLabel(unknown)
		if got != LabelOther {
			t.Errorf("FenceOpLabel(%q) = %q, want %q", unknown, got, LabelOther)
		}
		assertDocumented(t, "fence_op", documentedFenceOps, got)
	}
}

func assertDocumented(t *testing.T, label string, set map[string]struct{}, v string) {
	t.Helper()
	if _, ok := set[v]; !ok {
		t.Errorf("%s label %q is not in the documented set", label, v)
	}
}
