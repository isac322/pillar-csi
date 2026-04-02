package e2e

// image_pipeline_phase_test.go — AC8: explicit image-build/load pipeline phase.
//
// Verified behaviours:
//
//  1. pipelineStageOrder positions stageImageBuild strictly between
//     stageClusterCreate and stageBackendSetup, encoding the AC8 ordering
//     guarantee in the type system (cluster → images → backends).
//
//  2. resolveDockerBuildCache returns false by default (no env var).
//
//  3. resolveDockerBuildCache returns true when E2E_DOCKER_BUILD_CACHE is
//     "true" or "1".
//
//  4. resolveDockerBuildCache returns false for other non-true values.
//
//  5. buildCommandEnv(false) returns nil — docker build runs without any
//     injected env vars and therefore inherits the calling process environment
//     unmodified (including DOCKER_HOST).
//
//  6. buildCommandEnv(true) includes DOCKER_BUILDKIT=1 so that Docker BuildKit
//     layer caching is active when E2E_DOCKER_BUILD_CACHE=true.
//
//  7. bootstrapSuiteImages returns an error for a nil cluster state — the phase
//     cannot run without a preceding cluster-create phase.
//
//  8. bootstrapSuiteImages returns nil immediately when E2E_SKIP_IMAGE_BUILD=true
//     without modifying the provided cluster state.
//
//  9. e2eImageSpecs requires all three pillar-csi component images:
//     controller, agent, and node.  Each must have a non-empty Target and Name.
//
// 10. Each e2eImageSpec.Name must not contain the ":" character — the tag is
//     appended at runtime using resolveE2EImageTag, preventing accidental
//     double-tagging.
//
// 11. stageImageBuild constant value matches the string used in Makefile
//     comments and user-visible timing output ("image-build").
//
// 12. bootstrapSuiteImages accepts the *kindBootstrapState returned by
//     bootstrapSuiteCluster, enforcing phase ordering at the type level:
//     the caller cannot pass a nil or zero-value state because the function
//     validates it in the fast-fail path.

import (
	"context"
	"os"
	"testing"
)

// ── 1. pipelineStageOrder: stageImageBuild between cluster-create and backend ─

// TestAC8_PipelineStageOrder_ImageBuildBetweenClusterAndBackend verifies that
// stageImageBuild appears strictly between stageClusterCreate and
// stageBackendSetup in the canonical pipelineStageOrder slice.
//
// This is the authoritative AC8 ordering guarantee: docker build + kind load
// always run as Phase 3, after the Kind cluster is live (Phase 2) and before
// ZFS/LVM backend provisioning begins (Phase 4).
func TestAC8_PipelineStageOrder_ImageBuildBetweenClusterAndBackend(t *testing.T) {
	clusterIdx, imageIdx, backendIdx := -1, -1, -1

	for i, stage := range pipelineStageOrder {
		switch stage {
		case stageClusterCreate:
			clusterIdx = i
		case stageImageBuild:
			imageIdx = i
		case stageBackendSetup:
			backendIdx = i
		}
	}

	if clusterIdx < 0 {
		t.Fatalf("pipelineStageOrder missing %q", stageClusterCreate)
	}
	if imageIdx < 0 {
		t.Fatalf("pipelineStageOrder missing %q", stageImageBuild)
	}
	if backendIdx < 0 {
		t.Fatalf("pipelineStageOrder missing %q", stageBackendSetup)
	}

	// stageImageBuild must come after stageClusterCreate.
	if imageIdx <= clusterIdx {
		t.Errorf("[AC8] pipelineStageOrder: %q (idx=%d) must come after %q (idx=%d) — "+
			"images cannot be loaded before the Kind cluster is created",
			stageImageBuild, imageIdx, stageClusterCreate, clusterIdx)
	}

	// stageImageBuild must come before stageBackendSetup.
	if imageIdx >= backendIdx {
		t.Errorf("[AC8] pipelineStageOrder: %q (idx=%d) must come before %q (idx=%d) — "+
			"images must be loaded before backend provisioning",
			stageImageBuild, imageIdx, stageBackendSetup, backendIdx)
	}
}

// ── 2-4. resolveDockerBuildCache ──────────────────────────────────────────────

// TestAC8_ResolveDockerBuildCache_DefaultFalse verifies that build-cache mode
// is disabled by default (env var unset).
func TestAC8_ResolveDockerBuildCache_DefaultFalse(t *testing.T) {
	t.Setenv(dockerBuildCacheEnvVar, "")
	if resolveDockerBuildCache() {
		t.Errorf("[AC8] resolveDockerBuildCache()=true, want false when %s is empty",
			dockerBuildCacheEnvVar)
	}
}

// TestAC8_ResolveDockerBuildCache_TrueValues verifies that "true" and "1"
// both enable the build-cache mode (E2E_DOCKER_BUILD_CACHE).
func TestAC8_ResolveDockerBuildCache_TrueValues(t *testing.T) {
	for _, val := range []string{"true", "TRUE", "True", "1"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv(dockerBuildCacheEnvVar, val)
			if !resolveDockerBuildCache() {
				t.Errorf("[AC8] resolveDockerBuildCache()=false, want true for %q", val)
			}
		})
	}
}

// TestAC8_ResolveDockerBuildCache_FalseValues verifies that "false", "0",
// "no", and "off" keep build-cache mode disabled.
func TestAC8_ResolveDockerBuildCache_FalseValues(t *testing.T) {
	for _, val := range []string{"false", "0", "no", "off", "FALSE"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv(dockerBuildCacheEnvVar, val)
			if resolveDockerBuildCache() {
				t.Errorf("[AC8] resolveDockerBuildCache()=true, want false for %q", val)
			}
		})
	}
}

// ── 5-6. buildCommandEnv ──────────────────────────────────────────────────────

// TestAC8_BuildCommandEnv_CacheDisabled verifies that buildCommandEnv(false)
// returns nil. When nil, execCommandRunnerWithEnv delegates to the plain
// execCommandRunner which inherits os.Environ() unchanged — this ensures
// DOCKER_HOST flows through automatically without any special forwarding code.
func TestAC8_BuildCommandEnv_CacheDisabled(t *testing.T) {
	env := buildCommandEnv(false)
	if len(env) != 0 {
		t.Errorf("[AC8] buildCommandEnv(false) = %v, want nil (no extra env vars)", env)
	}
}

// TestAC8_BuildCommandEnv_CacheEnabled verifies that buildCommandEnv(true)
// includes DOCKER_BUILDKIT=1 so that Docker BuildKit layer caching is active
// when E2E_DOCKER_BUILD_CACHE=true.
func TestAC8_BuildCommandEnv_CacheEnabled(t *testing.T) {
	env := buildCommandEnv(true)
	if len(env) == 0 {
		t.Fatal("[AC8] buildCommandEnv(true) returned empty slice, want DOCKER_BUILDKIT=1")
	}

	found := false
	for _, v := range env {
		if v == "DOCKER_BUILDKIT=1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("[AC8] buildCommandEnv(true) = %v; missing DOCKER_BUILDKIT=1", env)
	}
}

// ── 7. bootstrapSuiteImages nil-state guard ───────────────────────────────────

// TestAC8_BootstrapSuiteImages_NilStateFails verifies that bootstrapSuiteImages
// returns a non-nil error when passed a nil cluster state. This enforces the
// pipeline ordering at runtime: the image phase cannot proceed without the
// cluster state produced by bootstrapSuiteCluster.
func TestAC8_BootstrapSuiteImages_NilStateFails(t *testing.T) {
	err := bootstrapSuiteImages(context.Background(), nil, os.Stderr)
	if err == nil {
		t.Error("[AC8] bootstrapSuiteImages(nil state) returned nil error, want non-nil")
	}
}

// ── 8. bootstrapSuiteImages skip path ────────────────────────────────────────

// TestAC8_BootstrapSuiteImages_SkipPreservesClusterState verifies that when
// E2E_SKIP_IMAGE_BUILD=true, bootstrapSuiteImages returns nil without
// modifying the provided cluster state. This preserves the state struct so
// that subsequent backend-provisioning (Phase 4) can proceed unaffected.
func TestAC8_BootstrapSuiteImages_SkipPreservesClusterState(t *testing.T) {
	t.Setenv(skipImageBuildEnvVar, "true")

	state := &kindBootstrapState{
		ClusterName:   "pillar-csi-e2e-ac8-test",
		KindBinary:    "kind",
		KubectlBinary: "kubectl",
		CreateTimeout: 1,
		DeleteTimeout: 1,
		SuiteRootDir:  t.TempDir(),
		WorkspaceDir:  t.TempDir(),
		LogsDir:       t.TempDir(),
		GeneratedDir:  t.TempDir(),
	}
	state.KubeconfigPath = state.GeneratedDir + "/kubeconfig"

	originalName := state.ClusterName
	originalKind := state.KindBinary

	err := bootstrapSuiteImages(t.Context(), state, nil)
	if err != nil {
		t.Fatalf("[AC8] bootstrapSuiteImages with E2E_SKIP_IMAGE_BUILD=true returned error: %v", err)
	}

	// Cluster state must be unchanged so Phase 4 (backend provisioning) can
	// still reference the correct cluster.
	if state.ClusterName != originalName {
		t.Errorf("[AC8] ClusterName changed after skip: got %q, want %q",
			state.ClusterName, originalName)
	}
	if state.KindBinary != originalKind {
		t.Errorf("[AC8] KindBinary changed after skip: got %q, want %q",
			state.KindBinary, originalKind)
	}
}

// ── 9. e2eImageSpecs contains required components ────────────────────────────

// TestAC8_ImageSpecs_RequiredTargets verifies that e2eImageSpecs lists all
// three pillar-csi component images (controller, agent, node) with non-empty
// Target and Name fields.
func TestAC8_ImageSpecs_RequiredTargets(t *testing.T) {
	required := map[string]bool{
		"controller": false,
		"agent":      false,
		"node":       false,
	}

	for _, img := range e2eImageSpecs {
		if img.Target == "" {
			t.Errorf("[AC8] e2eImageSpec has empty Target: %+v", img)
			continue
		}
		if img.Name == "" {
			t.Errorf("[AC8] e2eImageSpec has empty Name: %+v", img)
			continue
		}
		if _, ok := required[img.Target]; ok {
			required[img.Target] = true
		}
	}

	for target, found := range required {
		if !found {
			t.Errorf("[AC8] e2eImageSpecs missing required target %q", target)
		}
	}
}

// ── 10. e2eImageSpec.Name must not contain ":" ────────────────────────────────

// TestAC8_ImageSpecs_NamesHaveNoTag verifies that no e2eImageSpec.Name value
// contains ":" — the image tag is appended at runtime via resolveE2EImageTag,
// so a pre-tagged Name would produce malformed refs like "name:tag1:tag2".
func TestAC8_ImageSpecs_NamesHaveNoTag(t *testing.T) {
	for _, img := range e2eImageSpecs {
		for _, ch := range img.Name {
			if ch == ':' {
				t.Errorf("[AC8] e2eImageSpec{Target:%q}.Name = %q contains ':', "+
					"expected name without tag", img.Target, img.Name)
				break
			}
		}
	}
}

// ── 11. stageImageBuild constant value ───────────────────────────────────────

// TestAC8_StageImageBuild_ConstantValue verifies that the stageImageBuild
// constant equals the human-readable string "image-build" used in Makefile
// comments, pipeline timing output, and make test-e2e-profile reports.
func TestAC8_StageImageBuild_ConstantValue(t *testing.T) {
	const want = "image-build"
	if string(stageImageBuild) != want {
		t.Errorf("[AC8] stageImageBuild = %q, want %q", stageImageBuild, want)
	}
}

// ── 12. bootstrapSuiteImages type-enforces cluster-before-images ordering ────

// TestAC8_ImagePhase_ClusterStateIsExplicitInput verifies that
// bootstrapSuiteImages accepts *kindBootstrapState as its second argument —
// the exact type returned by bootstrapSuiteCluster.
//
// This is a compile-time enforcement of the pipeline ordering: you cannot
// call the image-build phase without first obtaining a cluster state from the
// cluster-creation phase.  The test exercises a non-nil state to confirm the
// function completes without crashing on the fast-skip path.
func TestAC8_ImagePhase_ClusterStateIsExplicitInput(t *testing.T) {
	t.Setenv(skipImageBuildEnvVar, "true")

	// Construct a minimal cluster state — equivalent to what bootstrapSuiteCluster
	// would return for an already-provisioned cluster.
	state := &kindBootstrapState{
		ClusterName:   "pillar-csi-e2e-ac8-typecheck",
		KindBinary:    "/usr/local/bin/kind",
		KubectlBinary: "/usr/local/bin/kubectl",
		CreateTimeout: 1,
		DeleteTimeout: 1,
		SuiteRootDir:  t.TempDir(),
		WorkspaceDir:  t.TempDir(),
		LogsDir:       t.TempDir(),
		GeneratedDir:  t.TempDir(),
	}
	state.KubeconfigPath = state.GeneratedDir + "/kubeconfig"

	// The function must accept *kindBootstrapState and succeed on the skip path.
	// If the type signature changes, this test will fail to compile.
	if err := bootstrapSuiteImages(t.Context(), state, nil); err != nil {
		t.Errorf("[AC8] bootstrapSuiteImages with skip=true returned unexpected error: %v", err)
	}
}
