package e2e

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/onsi/ginkgo/v2/types"
)

// ProfileCollector accumulates per-TC timing data during a suite run and
// writes a complete JSON ProfileReport to disk when Flush is called.
//
// Typical usage in a ReportAfterSuite hook:
//
//	collector := newProfileCollector(cfg.TimingReport.ProfilePath,
//	    cfg.TimingReport.BottleneckLimit,
//	    cfg.TimingReport.SlowSetupPhaseLimit)
//	if err := collector.Flush(suiteReport); err != nil {
//	    GinkgoWriter.Printf("profile flush error: %v\n", err)
//	}
type ProfileCollector struct {
	// path is the destination file for the JSON profile report.
	// An empty path is a no-op: Flush returns nil without writing.
	path string

	// bottleneckLimit is the number of slowest TCs to include in the
	// ProfileReport.Bottlenecks list. It defaults to profileReportBottleneckLimit (5).
	bottleneckLimit int

	// slowSetupPhaseLimit is the number of slowest setup phases to include in
	// ProfileReport.SlowSetupPhases (Sub-AC 6.3). Defaults to defaultSlowSetupPhaseLimit (5).
	slowSetupPhaseLimit int
}

// newProfileCollector creates a ProfileCollector that will write to path.
//
// bottleneckLimit controls how many slow TCs appear in the Bottlenecks section
// (the spec requires exactly 5 by default; pass 0 or a negative value to use
// the default).
//
// slowSetupPhaseLimit (variadic, Sub-AC 6.3) controls how many slow setup
// phases appear in SlowSetupPhases. Pass 0 or omit to use the default (5).
func newProfileCollector(path string, bottleneckLimit int, slowSetupPhaseLimit ...int) *ProfileCollector {
	if bottleneckLimit <= 0 {
		bottleneckLimit = profileReportBottleneckLimit
	}
	setupLimit := defaultSlowSetupPhaseLimit
	if len(slowSetupPhaseLimit) > 0 && slowSetupPhaseLimit[0] > 0 {
		setupLimit = slowSetupPhaseLimit[0]
	}
	return &ProfileCollector{
		path:                path,
		bottleneckLimit:     bottleneckLimit,
		slowSetupPhaseLimit: setupLimit,
	}
}

// Flush builds the complete ProfileReport from the Ginkgo suite report,
// computes the top-N (default 5) slowest TCs as BottleneckEntry items ordered
// by TotalNanos descending, collects the top-N slowest setup phases as
// SetupPhaseBottleneck items (Sub-AC 6.3), serialises the result to a single
// compact-JSON line, and writes it to the file path supplied when the collector
// was created.
//
// Flush is safe to call on a nil receiver or when the path is empty; in both
// cases it returns nil without writing anything.
//
// The parent directory of path must already exist; Flush does not create it.
// The destination file is created (or truncated) atomically using os.Create.
func (c *ProfileCollector) Flush(report types.Report) error {
	if c == nil || c.path == "" {
		return nil
	}

	// Validate that the parent directory exists before attempting to create
	// the output file. This surfaces misconfiguration early with a clear
	// error message rather than an opaque os.Create failure.
	dir := filepath.Dir(c.path)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("profile collector: output directory %s: %w", dir, err)
	}

	// Build the full ProfileReport.
	// buildProfileReport already handles:
	//   • harvesting TCProfile entries from Ginkgo spec reports
	//   • sorting TCs by duration descending (then by TCID ascending for ties)
	//   • capping the Bottlenecks list at c.bottleneckLimit entries
	//   • computing PctOfSuiteRuntime for each bottleneck entry
	//   • building SlowSetupPhases capped at c.slowSetupPhaseLimit (Sub-AC 6.3)
	pr := buildProfileReport(report, c.bottleneckLimit, c.slowSetupPhaseLimit)

	// Create (or truncate) the destination file.
	f, err := os.Create(c.path)
	if err != nil {
		return fmt.Errorf("profile collector: create %s: %w", c.path, err)
	}
	defer func() { _ = f.Close() }()

	// EncodeProfileReport writes a single compact-JSON line terminated by "\n".
	if err := EncodeProfileReport(f, pr); err != nil {
		return fmt.Errorf("profile collector: write profile to %s: %w", c.path, err)
	}

	return nil
}
