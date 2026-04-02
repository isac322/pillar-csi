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
}

// ─── Provisioning ─────────────────────────────────────────────────────────────

// bootstrapSuiteBackends provisions shared ZFS and LVM backend resources
// inside the Kind cluster's control-plane container.
//
// It is called once from runPrimary in suite_test.go after bootstrapSuiteCluster
// succeeds. The provisioned resources are registered with suiteInvocationTeardown
// so that cleanup runs before the Kind cluster is deleted.
//
// Provisioning is opportunistic:
//   - ZFS pool is created only when the "zfs" kernel module is loaded.
//   - LVM VG is created only when the "dm_thin_pool" kernel module is loaded.
//
// On success the provisioned resources are exported via os.Setenv so that
// ginkgo workers inherit them through os.Environ() in reexecViaGinkgoCLI.
//
// bootstrapSuiteBackends never returns an error when a backend module is
// absent — it logs a warning and skips that backend. It does return an error
// when a module is present but provisioning encounters an unexpected failure.
//
// The returned *suiteBackendState is nil only if clusterState is nil.
func bootstrapSuiteBackends(
	ctx context.Context,
	clusterState *kindBootstrapState,
	output io.Writer,
) (*suiteBackendState, error) {
	if clusterState == nil {
		return nil, errors.New("[AC5.2] bootstrapSuiteBackends: cluster state is nil")
	}
	if output == nil {
		output = io.Discard
	}

	nodeContainer := zfs.KindNodeContainerName(clusterState.ClusterName, 0)
	suffix := backendNameSuffix(clusterState.ClusterName)

	state := &suiteBackendState{NodeContainer: nodeContainer}

	// ── ZFS pool ─────────────────────────────────────────────────────────────
	//
	// The ZFS kernel module must be loaded on the host because Kind containers
	// share the host kernel. We check /proc/modules before attempting any
	// docker exec to give a clear diagnostic rather than a cryptic container
	// exec failure.
	//
	// We also tolerate the case where the ZFS userspace tools (zpool, zfs) are
	// not installed inside the Kind node container. The standard Kind node image
	// does not ship ZFS userspace utilities; a custom image or a privileged
	// install step is required.  When the binary is absent we log a warning and
	// skip ZFS provisioning exactly as if the kernel module were absent — the
	// ZFS-specific test cases will then be skipped at spec time due to the
	// absent PILLAR_E2E_ZFS_POOL env var.
	if isKernelModuleLoaded("zfs") {
		poolName := "pillar-e2e-zfs-" + suffix
		pool, err := zfs.CreatePool(ctx, zfs.CreatePoolOptions{
			NodeContainer: nodeContainer,
			PoolName:      poolName,
			SizeMiB:       512,
		})
		switch {
		case err == nil:
			// Verify the pool reached ONLINE state before exposing it to tests.
			// zpool create is synchronous but we confirm health explicitly so that
			// any unexpected degraded state (e.g. missing loop device) is caught
			// with a clear diagnostic rather than a cryptic test failure later.
			if verifyErr := zfs.VerifyOnline(ctx, nodeContainer, pool.PoolName); verifyErr != nil {
				// Pool creation succeeded but the pool is not ONLINE. Destroy it
				// and surface the error — test cases must never run against a
				// non-ONLINE pool.
				_ = pool.Destroy(ctx)
				return nil, fmt.Errorf("[AC5.2] ZFS pool %q on %s is not ONLINE after creation: %w",
					pool.PoolName, nodeContainer, verifyErr)
			}
			state.ZFSPool = pool
			_, _ = fmt.Fprintf(output,
				"[AC5.2] ZFS pool %q provisioned and ONLINE on container %s\n",
				pool.PoolName, nodeContainer)
		case isContainerToolNotFoundError(err):
			// Soft skip: zpool binary absent in the container — treat like
			// kernel module not loaded.  ZFS tests will be skipped at spec time.
			_, _ = fmt.Fprintf(output,
				"[AC5.2] zpool binary not found in container %s — "+
					"skipping ZFS pool provisioning (install zfsutils-linux in the Kind node image)\n",
				nodeContainer)
		default:
			// ZFS module is loaded AND zpool is present but provisioning failed —
			// this is a genuine error that must be surfaced.
			return nil, fmt.Errorf("[AC5.2] provision ZFS pool %q on %s: %w",
				poolName, nodeContainer, err)
		}
	} else {
		_, _ = fmt.Fprintf(output,
			"[AC5.2] zfs kernel module not loaded — skipping ZFS pool provisioning\n")
	}

	// ── LVM VG ───────────────────────────────────────────────────────────────
	//
	// LVM requires dm_thin_pool for thin-provisioning support (the primary LVM
	// mode used by pillar-csi). We skip silently when the module is absent.
	//
	// Same soft-skip logic as ZFS above: when lvm2 userspace tools (pvcreate,
	// vgcreate) are absent from the Kind container, we log a warning and skip
	// rather than failing the entire suite.
	if isKernelModuleLoaded("dm_thin_pool") {
		vgName := "pillar-e2e-lvm-" + suffix
		vg, err := lvm.CreateVG(ctx, lvm.CreateVGOptions{
			NodeContainer: nodeContainer,
			VGName:        vgName,
			SizeMiB:       512,
		})
		switch {
		case err == nil:
			// Verify the VG is writable and non-partial before exposing it to
			// tests.  vgcreate is synchronous but we confirm the attribute flags
			// explicitly so that any unexpected degraded state (e.g. partial PV,
			// exported VG) is caught here with a clear diagnostic rather than
			// producing cryptic failures inside individual test cases.
			if verifyErr := lvm.VerifyActive(ctx, nodeContainer, vg.VGName); verifyErr != nil {
				// VG was created but is not in an active/writable state. Destroy
				// it and surface the error — test cases must never run against a
				// non-active VG.
				_ = vg.Destroy(ctx)
				return nil, fmt.Errorf("[AC5.2] LVM VG %q on %s failed active check after creation: %w",
					vg.VGName, nodeContainer, verifyErr)
			}
			state.LVMVG = vg
			_, _ = fmt.Fprintf(output,
				"[AC5.2] LVM VG %q provisioned and active on container %s\n",
				vg.VGName, nodeContainer)
		case isContainerToolNotFoundError(err):
			// Soft skip: LVM userspace tools absent — treat like module not loaded.
			_, _ = fmt.Fprintf(output,
				"[AC5.2] pvcreate/vgcreate binary not found in container %s — "+
					"skipping LVM VG provisioning (install lvm2 in the Kind node image)\n",
				nodeContainer)
		default:
			// Module is loaded AND tools are present but provisioning failed.
			// Clean up any provisioned ZFS pool so we don't leak resources.
			if state.ZFSPool != nil {
				cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_ = state.ZFSPool.Destroy(cleanCtx)
				state.ZFSPool = nil
			}
			return nil, fmt.Errorf("[AC5.2] provision LVM VG %q on %s: %w",
				vgName, nodeContainer, err)
		}
	} else {
		_, _ = fmt.Fprintf(output,
			"[AC5.2] dm_thin_pool kernel module not loaded — skipping LVM VG provisioning\n")
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
		return fmt.Errorf("[AC5.2] export %s: %w", suiteBackendContainerEnvVar, err)
	}

	if s.ZFSPool != nil {
		if err := os.Setenv(suiteZFSPoolEnvVar, s.ZFSPool.PoolName); err != nil {
			return fmt.Errorf("[AC5.2] export %s: %w", suiteZFSPoolEnvVar, err)
		}
	}

	if s.LVMVG != nil {
		if err := os.Setenv(suiteLVMVGEnvVar, s.LVMVG.VGName); err != nil {
			return fmt.Errorf("[AC5.2] export %s: %w", suiteLVMVGEnvVar, err)
		}
	}

	// Mark provisioning complete so workers can assert the env is ready.
	if s.ZFSPool != nil || s.LVMVG != nil {
		if err := os.Setenv(suiteBackendProvisionedEnvVar, "1"); err != nil {
			return fmt.Errorf("[AC5.2] export %s: %w", suiteBackendProvisionedEnvVar, err)
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
			errs = append(errs, fmt.Errorf("[AC5.2] destroy ZFS pool %q: %w",
				s.ZFSPool.PoolName, err))
		} else {
			_, _ = fmt.Fprintf(output,
				"[AC5.2] ZFS pool %q destroyed on container %s\n",
				s.ZFSPool.PoolName, s.NodeContainer)

			// Sub-AC 4.3: verify the pool is absent after teardown so that a
			// silent failure in zpool destroy does not go undetected.
			exists, checkErr := zfs.PoolExists(ctx, s.ZFSPool.NodeContainer, s.ZFSPool.PoolName)
			if checkErr != nil {
				errs = append(errs, fmt.Errorf("[AC4.3] verify ZFS pool %q absent on %s: %w",
					s.ZFSPool.PoolName, s.ZFSPool.NodeContainer, checkErr))
			} else if exists {
				errs = append(errs, fmt.Errorf(
					"[AC4.3] ZFS pool %q still present on container %s after teardown",
					s.ZFSPool.PoolName, s.ZFSPool.NodeContainer))
			} else {
				_, _ = fmt.Fprintf(output,
					"[AC4.3] ZFS pool %q confirmed absent on container %s\n",
					s.ZFSPool.PoolName, s.ZFSPool.NodeContainer)
			}
		}
	}

	// Destroy LVM VG: this removes any LVs created by test cases before
	// removing the VG and PV, allowing the loop device to be detached.
	if s.LVMVG != nil {
		if err := s.LVMVG.Destroy(ctx); err != nil {
			errs = append(errs, fmt.Errorf("[AC5.2] destroy LVM VG %q: %w",
				s.LVMVG.VGName, err))
		} else {
			_, _ = fmt.Fprintf(output,
				"[AC5.2] LVM VG %q destroyed on container %s\n",
				s.LVMVG.VGName, s.NodeContainer)

			// Sub-AC 4.3: verify the VG is absent after teardown so that a
			// silent failure in vgremove does not go undetected.
			exists, checkErr := lvm.VGExists(ctx, s.LVMVG.NodeContainer, s.LVMVG.VGName)
			if checkErr != nil {
				errs = append(errs, fmt.Errorf("[AC4.3] verify LVM VG %q absent on %s: %w",
					s.LVMVG.VGName, s.LVMVG.NodeContainer, checkErr))
			} else if exists {
				errs = append(errs, fmt.Errorf(
					"[AC4.3] LVM VG %q still present on container %s after teardown",
					s.LVMVG.VGName, s.LVMVG.NodeContainer))
			} else {
				_, _ = fmt.Fprintf(output,
					"[AC4.3] LVM VG %q confirmed absent on container %s\n",
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
