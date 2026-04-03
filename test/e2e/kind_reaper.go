package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// orphanClusterPrefix is the naming prefix used by all pillar-csi E2E
	// Kind clusters. Any cluster whose name matches this prefix but was not
	// created by the current invocation is an orphan that should be reaped.
	orphanClusterPrefix = "pillar-csi-e2e-"

	// reaperTimeout is the maximum wall-clock time the orphan reaper is
	// allowed to consume. It covers the "kind get clusters" scan plus one
	// "kind delete cluster" call per orphan. In practice the list call
	// completes in < 1 s and each delete takes 2-4 s, keeping total time
	// well under the < 5 s budget when there are no orphans (the common
	// case on a clean host).
	reaperTimeout = 30 * time.Second
)

// reapOrphanedClusters scans for Kind clusters whose name starts with
// orphanClusterPrefix and deletes every match. It is called once by TestMain
// in the primary process, before bootstrapSuiteCluster creates a new cluster,
// so that stale clusters left by a previous SIGKILL'd run do not accumulate.
//
// The function is intentionally best-effort: individual delete failures are
// logged to output and do not abort the test run so that a single
// un-deletable orphan does not block all future test executions.
//
// DOCKER_HOST is inherited from the process environment automatically because
// execCommandRunner delegates to exec.CommandContext which inherits os.Environ.
func reapOrphanedClusters(output io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), reaperTimeout)
	defer cancel()

	kindBin := strings.TrimSpace(*e2eKindBinaryFlag)
	if kindBin == "" {
		kindBin = defaultKindBinary
	}

	runner := execCommandRunner{Output: output}
	_ = reapOrphanClustersWithRunner(ctx, &kindBinaryRunner{runner: runner, kindBin: kindBin}, output)
}

// kindBinaryRunner wraps execCommandRunner and substitutes the configured kind
// binary name so that reapOrphanClustersWithRunner can accept a commandRunner
// interface (which uses commandSpec.Name) without hard-coding the binary name.
type kindBinaryRunner struct {
	runner  commandRunner
	kindBin string
}

func (k *kindBinaryRunner) Run(ctx context.Context, spec commandSpec) (string, error) {
	// Override the binary name with the configured kind binary.
	spec.Name = k.kindBin
	return k.runner.Run(ctx, spec)
}

// pidFromClusterName extracts the process PID encoded in a pillar-csi-e2e
// cluster name of the form:
//
//	pillar-csi-e2e-p{pid}-{entropy}
//
// Returns 0 when the name does not match the expected pattern or the PID
// component cannot be parsed as a positive integer. A return value of 0 is
// treated conservatively as "unable to determine owning PID" and the cluster
// is not reaped.
func pidFromClusterName(name string) int {
	suffix := strings.TrimPrefix(name, orphanClusterPrefix)
	if suffix == name {
		return 0 // name does not start with orphanClusterPrefix
	}
	// suffix is "p{pid}-{entropy}" or an unrecognised pattern.
	// Extract the first dash-delimited token.
	dashIdx := strings.Index(suffix, "-")
	pidToken := suffix
	if dashIdx >= 0 {
		pidToken = suffix[:dashIdx]
	}
	// PID token must start with 'p' (the literal prefix added by newKindBootstrapState).
	if len(pidToken) < 2 || pidToken[0] != 'p' {
		return 0
	}
	pid, err := strconv.Atoi(pidToken[1:])
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// processAliveChecker is the function used to check whether a process is alive.
// It is a package-level variable so tests can substitute a fake implementation
// without forking real processes.
//
// The default implementation uses kill(pid, 0) semantics.
var processAliveChecker = defaultIsAliveProcess

// isOrphanedCluster reports whether the Kind cluster with the given name was
// created by a process that is no longer running, making it safe to delete
// without risking concurrent-invocation collisions.
//
// Returns false (do not delete) when:
//   - The name does not embed a recognisable PID component (unknown format).
//   - The owning process (by PID) is still alive.
//   - The liveness check is inconclusive (permission denied, etc.).
func isOrphanedCluster(name string) bool {
	pid := pidFromClusterName(name)
	if pid <= 0 {
		// Cannot determine ownership → be conservative, skip deletion.
		return false
	}
	return !processAliveChecker(pid)
}

// defaultIsAliveProcess reports whether the process with the given PID is
// currently running on this host.
//
// It uses (*os.Process).Signal(syscall.Signal(0)) — the traditional
// kill(pid, 0) probe — and interprets the return value conservatively:
//
//   - nil error           → process exists (signal delivery succeeded).
//   - os.ErrProcessDone   → process has exited (Go runtime sentinel).
//   - syscall.ESRCH       → no such process (raw kernel sentinel).
//   - syscall.EPERM       → process exists, no permission → treat as alive.
//   - other error         → treat conservatively as alive.
//
// Note: os.FindProcess on Linux always succeeds without checking whether the
// PID actually refers to a live process. The Signal(0) call performs the real
// liveness probe. Go 1.16+ wraps the POSIX ESRCH error as os.ErrProcessDone
// when the process struct's internal done flag is set; we check both.
func defaultIsAliveProcess(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		// os.FindProcess never errors on UNIX; conservatively assume alive.
		return true
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true // process exists
	}
	// Go 1.16+: process already exited (internal done flag set).
	if errors.Is(err, os.ErrProcessDone) {
		return false
	}
	// Raw kernel: no such process.
	if errors.Is(err, syscall.ESRCH) {
		return false
	}
	// EPERM or other: process exists but we can't signal it → treat as alive.
	return true
}

// reapOrphanClustersWithRunner is the testable core of reapOrphanedClusters.
// It accepts an injected commandRunner so unit tests can supply a fake runner
// without spawning real processes. The runner must use "kind" as the command
// name for all calls; callers that need a different binary should wrap their
// runner (see kindBinaryRunner above).
func reapOrphanClustersWithRunner(ctx context.Context, runner commandRunner, output io.Writer) error {
	// List all clusters currently known to the local Kind/Docker daemon.
	out, err := runner.Run(ctx, commandSpec{
		Name: "kind",
		Args: []string{"get", "clusters"},
	})
	if err != nil {
		combined := strings.ToLower(err.Error() + " " + out)
		if strings.Contains(combined, "no kind clusters found") {
			// No clusters at all — nothing to reap.
			return nil
		}
		_, _ = fmt.Fprintf(output,
			"[reaper] kind get clusters: %v — skipping orphan scan\n", err)
		return nil
	}

	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	// Some kind builds print "No kind clusters found." to stdout with exit 0.
	if strings.EqualFold(strings.TrimRight(out, "."), "no kind clusters found") {
		return nil
	}

	var orphans []string
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if !strings.HasPrefix(name, orphanClusterPrefix) {
			continue
		}
		// Sub-AC 2: Concurrent invocation safety — only reap clusters whose
		// owning process (encoded as p{pid} in the cluster name) is no longer
		// running. This prevents a concurrent go test invocation from deleting
		// another live invocation's cluster.
		if !isOrphanedCluster(name) {
			_, _ = fmt.Fprintf(output,
				"[reaper] skipping cluster %q — owning process is still running\n",
				name)
			continue
		}
		orphans = append(orphans, name)
	}

	if len(orphans) == 0 {
		return nil
	}

	_, _ = fmt.Fprintf(output,
		"[reaper] found %d orphaned kind cluster(s) matching %q — deleting\n",
		len(orphans), orphanClusterPrefix+"*")

	for _, name := range orphans {
		_, _ = fmt.Fprintf(output, "[reaper] deleting orphaned cluster %q\n", name)
		_, delErr := runner.Run(ctx, commandSpec{
			Name: "kind",
			Args: []string{"delete", "cluster", "--name", name},
		})
		if delErr != nil {
			_, _ = fmt.Fprintf(output,
				"[reaper] WARNING: failed to delete orphaned cluster %q: %v\n",
				name, delErr)
		} else {
			_, _ = fmt.Fprintf(output,
				"[reaper] deleted orphaned cluster %q\n", name)
		}
	}
	return nil
}
