package e2e

import (
	"context"
	"fmt"
	"strings"
)

// listClusters runs "kind get clusters" and returns the names of all running
// kind clusters. Returns an empty slice (not an error) when no clusters exist.
func (s *kindBootstrapState) listClusters(ctx context.Context, runner commandRunner) ([]string, error) {
	if s == nil {
		return nil, fmt.Errorf("kind bootstrap state is nil")
	}
	if runner == nil {
		return nil, fmt.Errorf("kind bootstrap runner is nil")
	}

	out, err := runner.Run(ctx, commandSpec{
		Name: s.KindBinary,
		Args: []string{"get", "clusters"},
	})
	if err != nil {
		// "kind get clusters" exits non-zero with a "No kind clusters found."
		// message in some builds; treat that as an empty list, not a real error.
		combined := strings.ToLower(err.Error() + " " + out)
		if strings.Contains(combined, "no kind clusters found") {
			return nil, nil
		}
		return nil, fmt.Errorf("list kind clusters: %w", err)
	}

	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	// Some builds print "No kind clusters found." to stdout with exit 0.
	if strings.EqualFold(strings.TrimRight(out, "."), "no kind clusters found") {
		return nil, nil
	}

	var names []string
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// verifyClusterPresent asserts that s.ClusterName appears in "kind get clusters".
// It is called after createCluster to confirm the cluster is actually running.
func (s *kindBootstrapState) verifyClusterPresent(ctx context.Context, runner commandRunner) error {
	clusters, err := s.listClusters(ctx, runner)
	if err != nil {
		return fmt.Errorf("verify cluster %q present: %w", s.ClusterName, err)
	}
	for _, name := range clusters {
		if name == s.ClusterName {
			return nil
		}
	}
	return fmt.Errorf(
		"kind cluster %q not found in [%s] — expected it to be running after creation",
		s.ClusterName,
		strings.Join(clusters, ", "),
	)
}

// verifyClusterAbsent asserts that s.ClusterName does NOT appear in
// "kind get clusters". It is called both before createCluster (pre-check) and
// after destroyCluster (post-check) to bound the cluster lifecycle to a single
// go test invocation.
func (s *kindBootstrapState) verifyClusterAbsent(ctx context.Context, runner commandRunner) error {
	clusters, err := s.listClusters(ctx, runner)
	if err != nil {
		return fmt.Errorf("verify cluster %q absent: %w", s.ClusterName, err)
	}
	for _, name := range clusters {
		if name == s.ClusterName {
			return fmt.Errorf(
				"kind cluster %q is still listed in 'kind get clusters' — expected it to be absent",
				s.ClusterName,
			)
		}
	}
	return nil
}
