package telemetry

import (
	"errors"
	"path/filepath"
	"strings"
	"syscall"
)

// LabelOther is the metric label value for any input outside a closed set.
const LabelOther = "other"

// configfsErrnos is the documented errno set of M8.
var configfsErrnos = map[syscall.Errno]string{
	syscall.EBUSY:  "EBUSY",
	syscall.EINVAL: "EINVAL",
	syscall.ENOENT: "ENOENT",
	syscall.EEXIST: "EEXIST",
	syscall.EPERM:  "EPERM",
	syscall.EACCES: "EACCES",
	syscall.ENOSPC: "ENOSPC",
	syscall.ENODEV: "ENODEV",
}

// ConfigfsErrnoLabel classifies a configfs primitive error for the errno
// label of pillar_csi_nvmet_configfs_errors_total (M8). The errno is taken
// from the *os.PathError / *os.LinkError chain; anything else, including an
// errno outside the documented set, is "other".
func ConfigfsErrnoLabel(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return LabelOther
	}
	if name, ok := configfsErrnos[errno]; ok {
		return name
	}
	return LabelOther
}

// Fencing operations (fence_op label of M7, pillar_csi.fence.op).
const (
	FenceOpGrant   = "grant"
	FenceOpRevoke  = "revoke"
	FenceOpDestroy = "destroy"
)

// Fencing decisions (decision label of M7, pillar_csi.fence.decision).
const (
	FenceAdmitNewLifecycle   = "admit_new_lifecycle"
	FenceAdmitAdvance        = "admit_advance"
	FenceAdmitSameGeneration = "admit_same_generation"
	FenceAdmitTerminalRetry  = "admit_terminal_retry"
	FenceRejectMissingToken  = "reject_missing_token"
	FenceRejectRetired       = "reject_retired"
	FenceRejectOtherOwner    = "reject_other_owner"
	FenceRejectEnded         = "reject_ended"
	FenceRejectSuperseded    = "reject_superseded"
	FenceRejectMarkChanged   = "reject_mark_changed"
	FenceMarkIOError         = "mark_io_error"
)

var fenceOps = setOf(FenceOpGrant, FenceOpRevoke, FenceOpDestroy)

var fenceDecisions = setOf(
	FenceAdmitNewLifecycle, FenceAdmitAdvance, FenceAdmitSameGeneration, FenceAdmitTerminalRetry,
	FenceRejectMissingToken, FenceRejectRetired, FenceRejectOtherOwner, FenceRejectEnded,
	FenceRejectSuperseded, FenceRejectMarkChanged, FenceMarkIOError,
)

// FenceOpLabel returns op when it is a FenceOp* constant, else "other".
func FenceOpLabel(op string) string { return closed(fenceOps, op) }

// FenceDecisionLabel returns decision when it is a Fence* decision constant,
// else "other".
func FenceDecisionLabel(decision string) string { return closed(fenceDecisions, decision) }

// Exec result label values (M9).
const (
	ExecResultOK         = "ok"
	ExecResultExitError  = "exit_error"
	ExecResultStartError = "start_error"
	ExecResultCanceled   = "canceled"
)

// Executables with an M9 subcommand label.
const (
	execZFS     = "zfs"
	execDmsetup = "dmsetup"
)

var execExecutables = setOf(
	execZFS, "lvs", "vgs", "lvcreate", "lvremove", "lvextend", execDmsetup,
	"mkfs.ext4", "mkfs.ext3", "mkfs.ext2", "mkfs.xfs", "resize2fs", "xfs_growfs",
)

var execSubcommands = map[string]map[string]struct{}{
	execZFS:     setOf("create", "destroy", "set", "get", "list"),
	execDmsetup: setOf("create", "reload", "resume", "suspend", "remove", "table", "info", "status"),
}

// ExecExecutableLabel maps a command name or path to the executable label of
// M9: its basename when in the documented set, else "other".
func ExecExecutableLabel(name string) string {
	return closed(execExecutables, filepath.Base(name))
}

// ExecSubcommandLabel maps an executable label and argv (without argv[0]) to
// the subcommand label of M9. Only zfs and dmsetup have subcommands; their
// first non-flag argument is kept when in the documented set, else "other".
// Every other executable yields "".
func ExecSubcommandLabel(executable string, args []string) string {
	subs, ok := execSubcommands[executable]
	if !ok {
		return ""
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		return closed(subs, a)
	}
	return LabelOther
}

// execRecordsArgs lists the executables whose argv may be recorded on a
// failed exec span. The mkfs and resize tools are excluded: their options
// come from user-supplied PVC annotations.
var execRecordsArgs = setOf(execZFS, "lvs", "vgs", "lvcreate", "lvremove", "lvextend", execDmsetup)

func setOf(vals ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(vals))
	for _, v := range vals {
		m[v] = struct{}{}
	}
	return m
}

func closed(set map[string]struct{}, v string) string {
	if _, ok := set[v]; ok {
		return v
	}
	return LabelOther
}
