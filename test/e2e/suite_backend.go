package e2e

// suite_backend.go — Sub-AC 5.2: shared backend provisioning for the Kind E2E suite.
//
// Acceptance criteria:
//
//  1. ZFS pool and LVM VG are provisioned ONCE per go test invocation inside
//     the Kind cluster's control-plane container — never per-test-case.
//  2. Provisioned resource names are exported as environment variables so that
//     all Ginkgo parallel workers inherit them automatically (via os.Environ()
//     in reexecViaGinkgoCLI).
//  3. Backend teardown (Destroy calls) runs BEFORE Kind cluster deletion so
//     that the Docker container is still alive when teardown commands execute.
//  4. Provisioning is opportunistic: if a kernel module is absent, the backend
//     is skipped rather than failing the suite.
//  5. DOCKER_HOST is read from the environment only — never hardcoded.
//
// Environment variable contract (inherited by ginkgo workers):
//
//	PILLAR_E2E_ZFS_POOL       — ZFS pool name in the Kind control-plane container
//	PILLAR_E2E_LVM_VG         — LVM Volume Group name in the Kind control-plane container
//	PILLAR_E2E_BACKEND_CONTAINER — Docker container name of the Kind node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/lvm"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/provisioner"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/zfs"
)

// ─── Environment variable names ──────────────────────────────────────────────

const (
	// suiteZFSPoolEnvVar carries the ZFS pool name provisioned by TestMain into
	// all ginkgo workers. E2E specs read this value to identify the shared pool.
	suiteZFSPoolEnvVar = "PILLAR_E2E_ZFS_POOL"

	// suiteLVMVGEnvVar carries the LVM Volume Group name provisioned by TestMain
	// into all ginkgo workers.
	suiteLVMVGEnvVar = "PILLAR_E2E_LVM_VG"

	// suiteLVMThinPoolEnvVar carries the LVM thin pool logical volume name
	// created inside the suite VG. Workers use this name to configure thin
	// provisioning in the LVM backend.
	suiteLVMThinPoolEnvVar = "PILLAR_E2E_LVM_THIN_POOL"

	// suiteBackendContainerEnvVar carries the Kind control-plane Docker container
	// name where backend resources were provisioned.
	suiteBackendContainerEnvVar = "PILLAR_E2E_BACKEND_CONTAINER"

	// suiteBackendProvisionedEnvVar is set to "1" when TestMain has successfully
	// provisioned at least one backend. Ginkgo workers check this to decide
	// whether to attempt backend-dependent operations.
	suiteBackendProvisionedEnvVar = "PILLAR_E2E_BACKEND_PROVISIONED"
)

// suiteOwnedBackendEnvVars lists the environment variable names that
// bootstrapSuiteBackends sets and resetSuiteInvocationEnvironment must unset
// at the beginning of each primary process run.
var suiteOwnedBackendEnvVars = []string{
	suiteZFSPoolEnvVar,
	suiteLVMVGEnvVar,
	suiteLVMThinPoolEnvVar,
	suiteBackendContainerEnvVar,
	suiteBackendProvisionedEnvVar,
}

// ─── Backend state ────────────────────────────────────────────────────────────

// suiteBackendState holds the shared ZFS/LVM backend resources provisioned
// once per test suite run (in TestMain.runPrimary) and shared across all test
// cases.
//
// Provisioning ZFS and LVM resources once at suite start eliminates the
// 2–5 second overhead per test case for dd + losetup + zpool/vgcreate.
//
// The struct is exported via env vars (suiteZFSPoolEnvVar, suiteLVMVGEnvVar)
// before ginkgo workers are spawned so workers need not re-create them.
type suiteBackendState struct {
	// NodeContainer is the Kind control-plane Docker container name where
	// backend resources were provisioned.
	NodeContainer string

	// ZFSPool is the ephemeral ZFS pool created inside NodeContainer.
	// Nil when the "zfs" kernel module was not loaded at provisioning time.
	ZFSPool *zfs.Pool

	// LVMVG is the ephemeral LVM Volume Group created inside NodeContainer.
	// Nil when the "dm_thin_pool" kernel module was not loaded at provisioning
	// time.
	LVMVG *lvm.VG

	// LVMThinPool is the name of the thin-pool logical volume created inside
	// LVMVG. Empty when thin-pool creation failed (non-fatal) or LVMVG is nil.
	// Workers use this name to configure the LVM backend in thin-provisioning
	// mode.
	LVMThinPool string
}

// ─── Provisioning ─────────────────────────────────────────────────────────────

// bootstrapSuiteBackends provisions shared backend resources inside the Kind
// cluster's control-plane container.
//
// It is called once from runPrimary in main_test.go after bootstrapSuiteCluster
// succeeds. The provisioned resources are registered with suiteInvocationTeardown
// so that cleanup runs before the Kind cluster is deleted.
//
// # Dependency injection
//
// The optional provisioners variadic parameter accepts any number of
// [provisioner.BackendProvisioner] implementations. When no provisioners are
// passed, the default pipeline is used: one [provisioner.ZFSProvisioner] and
// one [provisioner.LVMProvisioner] for the cluster's control-plane container.
//
// This allows callers to inject custom backends without modifying any
// framework code:
//
//	// Use default ZFS + LVM backends (no provisioners passed):
//	state, err := bootstrapSuiteBackends(ctx, clusterState, os.Stderr)
//
//	// Inject a custom backend via DI:
//	state, err := bootstrapSuiteBackends(ctx, clusterState, os.Stderr,
//	    &MyCustomProvisioner{NodeContainer: container},
//	)
//
// # Soft-skip semantics
//
// Provisioning is opportunistic: when a kernel module is absent or a required
// container tool is not installed, the backend is skipped (returns nil, nil)
// rather than failing the suite. Test cases that depend on an absent backend
// check whether the corresponding env var is set and skip accordingly.
//
// # Error handling
//
// When one or more backends return a hard error, bootstrapSuiteBackends
// destroys any already-provisioned resources (to avoid leaks), then returns
// a wrapped error with the [AC5] tag for log traceability.
//
// The returned *suiteBackendState is nil only if clusterState is nil.
func bootstrapSuiteBackends(
	ctx context.Context,
	clusterState *kindBootstrapState,
	output io.Writer,
	provisioners ...provisioner.BackendProvisioner,
) (*suiteBackendState, error) {
	if clusterState == nil {
		return nil, errors.New("[AC5] bootstrapSuiteBackends: cluster state is nil")
	}
	if output == nil {
		output = io.Discard
	}

	nodeContainer := zfs.KindNodeContainerName(clusterState.ClusterName, 0)
	suffix := backendNameSuffix(clusterState.ClusterName)

	// ── AC4: Install backend tools in the Kind container ──────────────────────
	//
	// The standard Kind node image does not ship ZFS userspace tools (zpool,
	// zfs) or LVM2 tools (pvcreate, vgcreate). Install them now via docker exec
	// so the provisioner pipeline below can create real ZFS pools and LVM VGs
	// inside the container.
	//
	// installKindContainerBackendTools is best-effort: if installation fails
	// (e.g. no network in CI, non-Debian image), it logs a warning and returns
	// nil — the provisioners detect the missing binaries and soft-skip.
	if installErr := installKindContainerBackendTools(ctx, nodeContainer, output); installErr != nil {
		// installKindContainerBackendTools only returns non-nil for internal
		// logic errors (e.g. nil context), not for apt failures — those are
		// handled internally and result in soft-skip at provisioning time.
		_, _ = fmt.Fprintf(output,
			"[AC4] warn: install backend tools in %s: %v\n", nodeContainer, installErr)
	}

	// ── AC9b: Set up NVMe-oF and iSCSI fabric backends ────────────────────────
	//
	// Configure kernel-level NVMe-oF TCP target and start tgtd iSCSI daemon
	// directly via docker exec. This avoids the multi-minute Kubernetes
	// DaemonSet deployment path and keeps fabric setup within the 2-minute
	// suite budget. AC9c checks these backends in SynchronizedBeforeSuite.
	if fabricErr := setupFabricBackends(ctx, nodeContainer, output); fabricErr != nil {
		_, _ = fmt.Fprintf(output,
			"[AC9b] warn: fabric backend setup in %s: %v\n", nodeContainer, fabricErr)
	}

	// ── Build the provisioner pipeline ────────────────────────────────────────
	//
	// When no provisioners are injected, construct the default pipeline with the
	// built-in ZFS and LVM backends. This preserves full backward compatibility:
	// existing callers that pass only (ctx, clusterState, output) still get the
	// same ZFS + LVM provisioning behaviour as before, while new callers can
	// supply custom backends without touching any framework code.
	pipeline := provisioner.NewPipeline()
	if len(provisioners) == 0 {
		pipeline.AddBackend(&provisioner.ZFSProvisioner{
			NodeContainer: nodeContainer,
			PoolName:      "pillar-e2e-zfs-" + suffix,
			// 4 GiB: large enough for the 1 GiB zvol created in verifyAgentLocalBackend
			// (ZFS refreservation requires free space >= zvol size) plus ZFS metadata.
			// Sparse image — truncate creates it instantly with no actual disk usage.
			SizeMiB: 4 * 1024,
		})
		pipeline.AddBackend(&provisioner.LVMProvisioner{
			NodeContainer: nodeContainer,
			VGName:        "pillar-e2e-lvm-" + suffix,
			// 100 GiB sparse image — truncate creates it instantly with no actual
			// disk usage. The large size allows realistic thin-provisioning tests
			// (e.g. 80 GiB virtual thin LVs) while keeping setup time < 1 second.
			SizeMiB: 100 * 1024,
		})
	} else {
		for _, p := range provisioners {
			pipeline.AddBackend(p)
		}
	}

	// ── Run the pipeline (concurrently) ──────────────────────────────────────
	//
	// Sub-AC 2.1 optimisation: ZFS pool creation and LVM VG creation are
	// completely independent operations. RunAllConcurrent provisions both in
	// parallel, cutting backend-setup wall-clock time roughly in half (from
	// sequential sum to the maximum of the two durations). The results slice
	// is still returned in registration order (ZFS at index 0, LVM at index 1)
	// regardless of which provisioner finishes first.
	results, provErr := pipeline.RunAllConcurrent(ctx, output)

	// On hard error: clean up any successfully provisioned resources before
	// returning so we do not leak ZFS pools or LVM VGs in the container.
	if provErr != nil {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanCancel()
		for _, r := range results {
			if r.Resource != nil && !r.Skipped && r.Err == nil {
				_ = r.Resource.Destroy(cleanCtx)
			}
		}
		return nil, fmt.Errorf("[AC5] backend provisioning: %w", provErr)
	}

	// ── Map results to suiteBackendState ─────────────────────────────────────
	//
	// Type-assert each provisioned resource back to its concrete type so the
	// typed ZFSPool / LVMVG fields of suiteBackendState can be populated.
	// Results from unknown backend types or from skipped / errored backends are
	// ignored — they are not part of the suite state.
	state := &suiteBackendState{NodeContainer: nodeContainer}

	for _, r := range results {
		if r.Skipped || r.Err != nil || r.Resource == nil {
			continue
		}
		switch r.BackendType {
		case "zfs":
			if pool, ok := r.Resource.(*zfs.Pool); ok {
				state.ZFSPool = pool
				_, _ = fmt.Fprintf(output,
					"[AC5] ZFS pool %q provisioned and ONLINE on container %s\n",
					pool.PoolName, nodeContainer)
			}
		case "lvm":
			if vg, ok := r.Resource.(*lvm.VG); ok {
				state.LVMVG = vg
				_, _ = fmt.Fprintf(output,
					"[AC5] LVM VG %q provisioned and active on container %s\n",
					vg.VGName, nodeContainer)

				// Create a thin pool inside the VG so that thin-provisioning TCs
				// (TC-E28.247, TC-E28.249, TC-E28.250, TC-E28.252, TC-E28.255, etc.)
				// can use ProvisionMode="thin" against a real thin pool.
				//
				// Thin pool size: 50 GiB (half of the 100 GiB sparse VG). The VG
				// image is sparse, so this does not consume host disk space.
				//
				// Non-fatal: if thin pool creation fails (e.g. dm_thin_pool module
				// not loaded), we log a warning but do not abort the suite.
				const thinPoolName = "pillar-e2e-pool"
				thinCtx, thinCancel := context.WithTimeout(ctx, 30*time.Second)
				thinErr := lvm.CreateThinPool(thinCtx, nodeContainer, vg.VGName, thinPoolName, 50*1024)
				thinCancel()
				if thinErr != nil {
					_, _ = fmt.Fprintf(output,
						"[AC5] warn: thin pool creation in VG %q failed (thin-mode TCs will be skipped): %v\n",
						vg.VGName, thinErr)
				} else {
					state.LVMThinPool = thinPoolName
					_, _ = fmt.Fprintf(output,
						"[AC5] LVM thin pool %q created in VG %q on container %s\n",
						thinPoolName, vg.VGName, nodeContainer)
				}
			}
		}
	}

	return state, nil
}

// ─── Environment export ───────────────────────────────────────────────────────

// exportBackendEnvironment exports the provisioned backend resource names as
// environment variables so that ginkgo parallel workers inherit them when
// reexecViaGinkgoCLI spawns them with os.Environ().
//
// Calling exportBackendEnvironment on a nil *suiteBackendState is a safe no-op
// that returns nil — this handles the case where all backends were skipped
// due to absent kernel modules.
func (s *suiteBackendState) exportBackendEnvironment() error {
	if s == nil {
		return nil
	}

	if err := os.Setenv(suiteBackendContainerEnvVar, s.NodeContainer); err != nil {
		return fmt.Errorf("[AC5] export %s: %w", suiteBackendContainerEnvVar, err)
	}

	if s.ZFSPool != nil {
		if err := os.Setenv(suiteZFSPoolEnvVar, s.ZFSPool.PoolName); err != nil {
			return fmt.Errorf("[AC5] export %s: %w", suiteZFSPoolEnvVar, err)
		}
	}

	if s.LVMVG != nil {
		if err := os.Setenv(suiteLVMVGEnvVar, s.LVMVG.VGName); err != nil {
			return fmt.Errorf("[AC5] export %s: %w", suiteLVMVGEnvVar, err)
		}
		if s.LVMThinPool != "" {
			if err := os.Setenv(suiteLVMThinPoolEnvVar, s.LVMThinPool); err != nil {
				return fmt.Errorf("[AC5] export %s: %w", suiteLVMThinPoolEnvVar, err)
			}
		}
	}

	// Mark provisioning complete so workers can assert the env is ready.
	if s.ZFSPool != nil || s.LVMVG != nil {
		if err := os.Setenv(suiteBackendProvisionedEnvVar, "1"); err != nil {
			return fmt.Errorf("[AC5] export %s: %w", suiteBackendProvisionedEnvVar, err)
		}
	}

	return nil
}

// ─── Teardown ─────────────────────────────────────────────────────────────────

// teardown destroys all provisioned backend resources inside the Kind container.
//
// It runs BEFORE Kind cluster deletion (enforced by the ordering in
// invocationTeardown.Cleanup) so that the Docker container is still alive when
// teardown commands execute.
//
// Calling teardown on a nil *suiteBackendState is a safe no-op.
// All teardown steps execute even when an individual step fails; errors are
// collected and returned together via errors.Join.
//
// Sub-AC 4.3: after each successful Destroy call, teardown verifies resource
// absence using PoolExists / VGExists and returns an error if the resource
// is still detectable in the container.
func (s *suiteBackendState) teardown(ctx context.Context, output io.Writer) error {
	if s == nil {
		return nil
	}
	if output == nil {
		output = io.Discard
	}

	var errs []error

	// Destroy ZFS pool first: this removes all zvols / datasets that test cases
	// may have created inside the pool, releasing their loop devices cleanly.
	if s.ZFSPool != nil {
		if err := s.ZFSPool.Destroy(ctx); err != nil {
			errs = append(errs, fmt.Errorf("[AC5] destroy ZFS pool %q: %w",
				s.ZFSPool.PoolName, err))
		} else {
			_, _ = fmt.Fprintf(output,
				"[AC5] ZFS pool %q destroyed on container %s\n",
				s.ZFSPool.PoolName, s.NodeContainer)

			// Sub-AC 4.3: verify the pool is absent after teardown so that a
			// silent failure in zpool destroy does not go undetected.
			exists, checkErr := zfs.PoolExists(ctx, s.ZFSPool.NodeContainer, s.ZFSPool.PoolName)
			if checkErr != nil {
				errs = append(errs, fmt.Errorf("[AC4] verify ZFS pool %q absent on %s: %w",
					s.ZFSPool.PoolName, s.ZFSPool.NodeContainer, checkErr))
			} else if exists {
				errs = append(errs, fmt.Errorf(
					"[AC4] ZFS pool %q still present on container %s after teardown",
					s.ZFSPool.PoolName, s.ZFSPool.NodeContainer))
			} else {
				_, _ = fmt.Fprintf(output,
					"[AC4] ZFS pool %q confirmed absent on container %s\n",
					s.ZFSPool.PoolName, s.ZFSPool.NodeContainer)
			}
		}
	}

	// Destroy LVM VG: this removes any LVs created by test cases before
	// removing the VG and PV, allowing the loop device to be detached.
	if s.LVMVG != nil {
		if err := s.LVMVG.Destroy(ctx); err != nil {
			errs = append(errs, fmt.Errorf("[AC5] destroy LVM VG %q: %w",
				s.LVMVG.VGName, err))
		} else {
			_, _ = fmt.Fprintf(output,
				"[AC5] LVM VG %q destroyed on container %s\n",
				s.LVMVG.VGName, s.NodeContainer)

			// Sub-AC 4.3: verify the VG is absent after teardown so that a
			// silent failure in vgremove does not go undetected.
			exists, checkErr := lvm.VGExists(ctx, s.LVMVG.NodeContainer, s.LVMVG.VGName)
			if checkErr != nil {
				errs = append(errs, fmt.Errorf("[AC4] verify LVM VG %q absent on %s: %w",
					s.LVMVG.VGName, s.LVMVG.NodeContainer, checkErr))
			} else if exists {
				errs = append(errs, fmt.Errorf(
					"[AC4] LVM VG %q still present on container %s after teardown",
					s.LVMVG.VGName, s.LVMVG.NodeContainer))
			} else {
				_, _ = fmt.Fprintf(output,
					"[AC4] LVM VG %q confirmed absent on container %s\n",
					s.LVMVG.VGName, s.LVMVG.NodeContainer)
			}
		}
	}

	return errors.Join(errs...)
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

// isKernelModuleLoaded reports whether the given kernel module is currently
// loaded by parsing /proc/modules. It normalises hyphens to underscores to
// match the kernel's internal representation.
//
// Returns false when /proc/modules cannot be read (e.g. non-Linux OS), which
// is treated the same as "module not loaded" for provisioning purposes.
func isKernelModuleLoaded(name string) bool {
	data, err := os.ReadFile("/proc/modules")
	if err != nil {
		return false
	}
	target := strings.ReplaceAll(name, "-", "_")
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			loaded := strings.ReplaceAll(fields[0], "-", "_")
			if loaded == target {
				return true
			}
		}
	}
	return false
}

// backendNameSuffix derives a short (≤ 8 char), DNS-safe suffix from the
// cluster name for use in backend resource names (ZFS pool names, LVM VG names).
//
// Kind cluster names have the form "pillar-csi-e2e-p<pid>-<8hexchars>".
// We take the last 8 characters of the cluster name. If the first character
// happens to be a digit (DNS labels must start with a letter), we prepend "s".
//
// Examples:
//
//	"pillar-csi-e2e-p12345-abcd1234" → "abcd1234"
//	"pillar-csi-e2e-p12345-1234abcd" → "s234abcd"
func backendNameSuffix(clusterName string) string {
	if len(clusterName) == 0 {
		return "default"
	}
	n := len(clusterName)
	if n <= 8 {
		suffix := clusterName
		if suffix[0] >= '0' && suffix[0] <= '9' {
			suffix = "s" + suffix[1:]
		}
		return suffix
	}
	suffix := clusterName[n-8:]
	if suffix[0] >= '0' && suffix[0] <= '9' {
		suffix = "s" + suffix[1:]
	}
	return suffix
}

// isContainerToolNotFoundError reports whether err indicates that a required
// binary (e.g. "zpool", "pvcreate") was absent from the container's PATH.
//
// This error originates from "docker exec" when the Docker daemon's OCI
// runtime cannot start the requested process: the stderr message contains
// "executable file not found in $PATH" (as produced by runc) or the variant
// "exec: … not found" (as produced by containerd/cri).
//
// When this condition is detected, the caller should treat the backend as
// unavailable (soft skip) rather than failing the suite, because the standard
// Kind node image does not ship ZFS or LVM2 userspace tools.
func isContainerToolNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "executable file not found in $path") ||
		strings.Contains(msg, "executable file not found") ||
		strings.Contains(msg, "no such file or directory") ||
		// containerd / runc variant: "exec: \"zpool\": executable file not found"
		(strings.Contains(msg, "exec:") && strings.Contains(msg, "not found")) ||
		// Generic "command not found" from shell wrappers
		strings.Contains(msg, "command not found")
}
