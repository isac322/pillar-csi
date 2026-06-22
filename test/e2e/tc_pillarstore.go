package e2e

// tc_pillarstore.go — Sub-AC 5.3: per-TC PillarStore K8s CR provisioning.
//
// Every test case that needs LVM-backed storage receives a dedicated PillarStore
// CR whose name and LVM VG name are derived from the test's DerivedNamespace
// (names.Namespace(tcID)).  Derivation from the namespace guarantees:
//
//   - Globally unique names per TC (the namespace embeds a SHA-256 hash of the
//     raw TC ID, so two different TC IDs always yield distinct names).
//   - Stable names within a single TC lifecycle — the same TC ID always maps to
//     the same PillarStore / VG name, which aids debugging and log tracing.
//   - No sharing across parallel TCs — concurrent runners all execute different
//     TC IDs, so their derived namespaces — and therefore their pool/VG names —
//     are always distinct.
//
// # Naming scheme
//
//	PillarStore name:  "pp-" + last 12 chars of DerivedNamespace
//	LVM VG name:      "vg-" + last 12 chars of DerivedNamespace
//
// Example: DerivedNamespace = "e2e-tc-e3-1-ab12cd34" (20 chars)
//   PillarStore name = "pp-e3-1-ab12cd34"   (16 chars, ≤ 63)
//   LVM VG name     = "vg-e3-1-ab12cd34"   (16 chars, ≤ 127)
//
// # Lifecycle
//
//	ProvisionPerTestPillarStore (BeforeEach phase):
//	  Creates the PillarStore CR via the provided k8sClient and registers a
//	  teardown callback with the TC scope via TrackBackendRecord.
//
//	scope.Close() (AfterEach / DeferCleanup phase):
//	  Deletes the PillarStore CR and verifies it is absent before closing.

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pillarv1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// PillarStoreHandle is a per-TC handle for a PillarStore K8s CR provisioned
// by ProvisionPerTestPillarStore.
type PillarStoreHandle struct {
	// PoolName is the Kubernetes resource name of the PillarStore CR.
	// Derived from scope.DerivedNamespace; globally unique per TC.
	PoolName string

	// VGName is the LVM Volume Group name embedded in the PillarStore spec.
	// Derived from scope.DerivedNamespace; globally unique per TC.
	VGName string

	// AgentRef is the PillarAgent name this pool references.
	AgentRef string
}

// ProvisionPerTestPillarStore creates a dedicated PillarStore K8s CR for the
// given test case scope and registers automatic teardown with the scope.
//
// Both the PillarStore name and the LVM VG name are derived from
// scope.DerivedNamespace (computed by names.Namespace from the TC ID), so:
//
//   - No two parallel tests share the same PillarStore or VG name.
//   - The names are deterministic across runs of the same TC ID.
//   - The names are traceable to the TC ID for debugging.
//
// The created PillarStore uses lvm-lv backend type with linear provisioning.
// The VG is assumed to be pre-created by suite-level backend provisioning
// (Sub-AC 5.2) or by individual test fixtures; this function only manages
// the Kubernetes CR lifecycle.
//
// Teardown: the PillarStore CR is deleted and verified absent when scope.Close()
// is called (the DeferCleanup / AfterEach phase), regardless of spec outcome.
//
// Returns an error if:
//   - scope, k8sClient, or agentRef is nil/empty
//   - the PillarStore CR cannot be created (e.g., API server unreachable)
//   - teardown registration fails (scope already closed)
func ProvisionPerTestPillarStore(
	ctx context.Context,
	scope *TestCaseScope,
	k8sClient client.Client,
	agentRef string,
) (*PillarStoreHandle, error) {
	if scope == nil {
		return nil, fmt.Errorf("[AC5.3] ProvisionPerTestPillarStore: scope is required")
	}
	if k8sClient == nil {
		return nil, fmt.Errorf("[AC5.3] ProvisionPerTestPillarStore: k8sClient is required")
	}
	if strings.TrimSpace(agentRef) == "" {
		return nil, fmt.Errorf("[AC5.3] ProvisionPerTestPillarStore: agentRef is required")
	}

	poolName := pillarStoreName(scope.DerivedNamespace)
	vgName := perTestLVMVGName(scope.DerivedNamespace)

	pool := &pillarv1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{
			Name: poolName,
		},
		Spec: pillarv1.PillarStoreSpec{
			AgentRef: agentRef,
			Backend: pillarv1.BackendSpec{
				Type: pillarv1.BackendTypeLVMLV,
				LVM: &pillarv1.LVMBackendConfig{
					VolumeGroup:      vgName,
					ProvisioningMode: pillarv1.LVMProvisioningModeLinear,
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, pool); err != nil {
		return nil, fmt.Errorf("[AC5.3] create PillarStore %q for TC %q: %w",
			poolName, scope.TCID, err)
	}

	handle := &PillarStoreHandle{
		PoolName: poolName,
		VGName:   vgName,
		AgentRef: agentRef,
	}

	if err := scope.TrackBackendRecord("pillarstore:"+poolName, PathResourceSpec{
		Path: poolName, // logical identifier used for display in teardown errors
		Cleanup: func() error {
			deleteCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			toDelete := &pillarv1.PillarStore{}
			toDelete.Name = poolName
			if delErr := k8sClient.Delete(deleteCtx, toDelete); delErr != nil {
				// Not-found is not an error — another teardown path may have
				// already deleted the resource.
				if !apierrors.IsNotFound(delErr) {
					return fmt.Errorf("[AC5.3] delete PillarStore %q for TC %q: %w",
						poolName, scope.TCID, delErr)
				}
			}
			return nil
		},
		IsPresent: func() (bool, error) {
			checkCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			existing := &pillarv1.PillarStore{}
			err := k8sClient.Get(checkCtx, client.ObjectKey{Name: poolName}, existing)
			if err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, fmt.Errorf("[AC5.3] check PillarStore %q for TC %q: %w",
					poolName, scope.TCID, err)
			}
			return true, nil
		},
	}); err != nil {
		// Registration failed — clean up the PillarStore we just created so we
		// don't leak it.
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = k8sClient.Delete(cleanCtx, pool)
		return nil, fmt.Errorf("[AC5.3] register teardown for PillarStore %q: %w",
			poolName, err)
	}

	return handle, nil
}

// ─── Name derivation helpers ──────────────────────────────────────────────────

// pillarStoreName derives a PillarStore K8s resource name from the test's
// derived namespace.
//
// Format: "pp-<suffix>" where suffix is the last 12 characters of the derived
// namespace stripped of leading/trailing hyphens.
//
// The "pp-" prefix (PillarStore) distinguishes pool names from VG names and
// other cluster-scoped resources in the same cluster.
//
// Examples:
//
//	"e2e-tc-e3-1-ab12cd34" → "pp-1-ab12cd34"    (13 chars)
//	"e2e-tc-e30-1-ff00ee11" → "pp-0-1-ff00ee11"  (15 chars, capped at 15 max)
//	"e2e-tc-a-12345678"     → "pp-a-12345678"    (13 chars)
//
// PillarStore resources are cluster-scoped; names must be valid DNS labels
// (≤ 63 chars, matches [a-z0-9]([-a-z0-9]*[a-z0-9])?).  The derived namespace
// already satisfies this constraint; the "pp-" prefix and 12-char suffix keep
// the total ≤ 15 chars.
func pillarStoreName(derivedNamespace string) string {
	return "pp-" + namespaceSuffix(derivedNamespace, 12)
}

// perTestLVMVGName derives an LVM Volume Group name from the test's derived
// namespace.
//
// Format: "vg-<suffix>" where suffix is the last 12 characters of the derived
// namespace stripped of leading/trailing hyphens.
//
// LVM VG names have a 127-character limit and must not start with a hyphen.
// The DNS-safe derived namespace satisfies these constraints; the "vg-" prefix
// and 12-char suffix keep the total ≤ 15 chars.
//
// Examples:
//
//	"e2e-tc-e3-1-ab12cd34" → "vg-1-ab12cd34"
//	"e2e-tc-lvm-test-ff00ee11" → "vg-t-ff00ee11"
func perTestLVMVGName(derivedNamespace string) string {
	return "vg-" + namespaceSuffix(derivedNamespace, 12)
}

// namespaceSuffix returns the last n characters of the derived namespace,
// stripped of leading/trailing hyphens to satisfy OS naming constraints.
//
// The derived namespace produced by names.Namespace(tcID) has the format
// "e2e-tc-<slug>-<hash8>" where hash8 is a stable SHA-256 prefix.  Taking the
// last n characters captures the hash (which provides uniqueness) together with
// a short human-readable slug fragment (which aids debugging).
//
// If the derived namespace is shorter than n characters, the entire string is
// returned (stripped of leading/trailing hyphens).  An empty or all-hyphens
// input returns "x" so the result is always non-empty.
func namespaceSuffix(ns string, n int) string {
	if ns == "" {
		return "x"
	}
	var suffix string
	if len(ns) <= n {
		suffix = strings.Trim(ns, "-")
	} else {
		suffix = strings.Trim(ns[len(ns)-n:], "-")
	}
	if suffix == "" {
		return "x"
	}
	return suffix
}
