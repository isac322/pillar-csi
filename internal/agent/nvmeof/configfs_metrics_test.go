/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// M8 label tests for pillar_csi_nvmet_configfs_errors_total.  They are NOT
// parallel: the counter is process-global, so deltas are meaningful only
// while the package's parallel tests are paused.

package nvmeof

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func configfsErrorCount(op, errno string) float64 {
	return testutil.ToFloat64(configfsErrors.WithLabelValues(op, errno))
}

// The disable write and the second, idempotent Remove are best-effort
// teardown steps: their failures are tolerated and must not be counted.
func TestConfigfsErrors_ToleratedTeardownNotCounted(t *testing.T) {
	tgt := orderingTarget(t.TempDir(), "tolerated")
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Make the enable attribute unwritable: os.WriteFile on a directory
	// fails with EISDIR.  Remove tolerates and discards that failure.
	enablePath := filepath.Join(tgt.namespaceDir(), "enable")
	if err := os.Remove(enablePath); err != nil {
		t.Fatalf("remove enable attr: %v", err)
	}
	if err := os.Mkdir(enablePath, 0o750); err != nil {
		t.Fatalf("replace enable attr with dir: %v", err)
	}

	writeOther := configfsErrorCount(configfsOpWrite, "other")
	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove must tolerate the failed disable write: %v", err)
	}
	if got := configfsErrorCount(configfsOpWrite, "other"); got != writeOther {
		t.Fatalf("tolerated disable-write failure counted: write/other %v -> %v", writeOther, got)
	}

	// A second Remove finds the namespace already gone: the disable write
	// fails ENOENT.  That tolerated miss must not be counted either.
	writeENOENT := configfsErrorCount(configfsOpWrite, "ENOENT")
	if err := tgt.Remove(); err != nil {
		t.Fatalf("idempotent Remove: %v", err)
	}
	if got := configfsErrorCount(configfsOpWrite, "ENOENT"); got != writeENOENT {
		t.Fatalf("tolerated ENOENT disable write counted: write/ENOENT %v -> %v", writeENOENT, got)
	}
}

// A leftover entry inside a kernel-managed default group makes its
// best-effort rmdir fail (EPERM on configfs, ENOTEMPTY here); only the
// propagated failure of the subsystem rmdir is counted.
func TestConfigfsErrors_ToleratedRmdirNotCounted(t *testing.T) {
	tgt := orderingTarget(t.TempDir(), "tolerated-rmdir")
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	stale := filepath.Join(tgt.subsystemDir(), "namespaces", "stale")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatalf("plant stale entry: %v", err)
	}

	rmdirOther := configfsErrorCount(configfsOpRmdir, "other")
	if err := tgt.Remove(); err == nil {
		t.Fatal("Remove must propagate the subsystem rmdir failure")
	}
	if got := configfsErrorCount(configfsOpRmdir, "other") - rmdirOther; got != 1 {
		t.Fatalf("rmdir/other delta = %v, want 1 (subsystem rmdir counted, namespaces rmdir tolerated)", got)
	}
}

// Errors a caller sees are still counted: no silent failures.
func TestConfigfsErrors_PrimitiveFailureCounted(t *testing.T) {
	writeENOENT := configfsErrorCount(configfsOpWrite, "ENOENT")
	err := writeFile(filepath.Join(t.TempDir(), "missing", "attr"), "1")
	if err == nil {
		t.Fatal("writeFile under a missing directory must fail")
	}
	if got := configfsErrorCount(configfsOpWrite, "ENOENT") - writeENOENT; got != 1 {
		t.Fatalf("write/ENOENT delta = %v, want 1", got)
	}
}
