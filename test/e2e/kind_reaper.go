package e2e

import (
	"context"
	"fmt"
	"io"
	"strings"
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
		if strings.HasPrefix(name, orphanClusterPrefix) {
			orphans = append(orphans, name)
		}
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
