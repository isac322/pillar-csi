package e2e

import (
	"context"
	"os"
	"testing"
)

// ─── resolveE2EImageTag ───────────────────────────────────────────────────────

func TestResolveE2EImageTag_Default(t *testing.T) {
	t.Parallel()
	// Use the value-based helper to avoid t.Setenv incompatibility with t.Parallel.
	got := resolveE2EImageTagFromValue("")
	if got != defaultE2EImageTag {
		t.Errorf("resolveE2EImageTagFromValue(\"\")=%q, want %q", got, defaultE2EImageTag)
	}
}

func TestResolveE2EImageTag_EnvSet(t *testing.T) {
	t.Parallel()
	got := resolveE2EImageTagFromValue("v1.2.3")
	if got != "v1.2.3" {
		t.Errorf("resolveE2EImageTagFromValue(\"v1.2.3\")=%q, want %q", got, "v1.2.3")
	}
}

func TestResolveE2EImageTag_Whitespace(t *testing.T) {
	t.Parallel()
	got := resolveE2EImageTagFromValue("  my-tag  ")
	if got != "my-tag" {
		t.Errorf("resolveE2EImageTagFromValue(\"  my-tag  \")=%q, want %q", got, "my-tag")
	}
}

// ─── resolveSkipImageBuild ───────────────────────────────────────────────────

func TestResolveSkipImageBuild_False(t *testing.T) {
	t.Parallel()
	if resolveSkipImageBuildFromValue("") {
		t.Error("resolveSkipImageBuildFromValue(\"\")=true, want false when val is empty")
	}
}

func TestResolveSkipImageBuild_True(t *testing.T) {
	t.Parallel()
	for _, val := range []string{"true", "TRUE", "True", "1"} {
		val := val
		t.Run(val, func(t *testing.T) {
			t.Parallel()
			if !resolveSkipImageBuildFromValue(val) {
				t.Errorf("resolveSkipImageBuildFromValue(%q)=false, want true", val)
			}
		})
	}
}

func TestResolveSkipImageBuild_FalseValues(t *testing.T) {
	t.Parallel()
	for _, val := range []string{"false", "0", "no", "off"} {
		val := val
		t.Run(val, func(t *testing.T) {
			t.Parallel()
			if resolveSkipImageBuildFromValue(val) {
				t.Errorf("resolveSkipImageBuildFromValue(%q)=true, want false", val)
			}
		})
	}
}

// ─── findRepoRoot ─────────────────────────────────────────────────────────────

func TestFindRepoRoot_Success(t *testing.T) {
	t.Parallel()
	// Running inside the repo tree, so findRepoRoot must succeed and the
	// returned directory must contain go.mod.
	root, err := findRepoRoot()
	if err != nil {
		t.Fatalf("findRepoRoot() error=%v, want nil", err)
	}
	if _, statErr := os.Stat(root + "/go.mod"); statErr != nil {
		t.Errorf("findRepoRoot()=%q: no go.mod found: %v", root, statErr)
	}
}

// ─── bootstrapSuiteImages ────────────────────────────────────────────────────

func TestBootstrapSuiteImages_NilState(t *testing.T) {
	t.Parallel()
	err := bootstrapSuiteImages(context.Background(), nil, os.Stderr)
	if err == nil {
		t.Error("bootstrapSuiteImages(nil state) returned nil, want error")
	}
}

// TestBootstrapSuiteImages_SkipWhenEnvSet verifies that bootstrapSuiteImages
// takes the fast-skip path when the skip flag is set, returning nil without
// attempting any docker build or kind load commands.
//
// Uses bootstrapSuiteImagesWithSkip(skip=true) directly to avoid t.Setenv
// (which is incompatible with t.Parallel in Go 1.21+).
func TestBootstrapSuiteImages_SkipWhenEnvSet(t *testing.T) {
	t.Parallel()
	state := &kindBootstrapState{
		ClusterName:   "test-cluster",
		KindBinary:    "kind",
		KubectlBinary: "kubectl",
		CreateTimeout: 1,
		DeleteTimeout: 1,
		SuiteRootDir:  t.TempDir(),
		WorkspaceDir:  t.TempDir(),
		LogsDir:       t.TempDir(),
		GeneratedDir:  t.TempDir(),
	}
	// We set KubeconfigPath to a file under GeneratedDir so validate() passes.
	state.KubeconfigPath = state.GeneratedDir + "/kubeconfig"

	// Pass skip=true directly to test the fast-path without env var manipulation.
	err := bootstrapSuiteImagesWithSkip(context.Background(), state, os.Stderr, true)
	if err != nil {
		t.Errorf("bootstrapSuiteImagesWithSkip with skip=true returned error: %v", err)
	}
}

// ─── e2eImageSpecs sanity ─────────────────────────────────────────────────────

func TestE2EImageSpecs_NotEmpty(t *testing.T) {
	t.Parallel()
	if len(e2eImageSpecs) == 0 {
		t.Error("e2eImageSpecs is empty; at least controller/agent/node must be defined")
	}
	for _, img := range e2eImageSpecs {
		if img.Target == "" {
			t.Errorf("e2eImageSpec has empty Target: %+v", img)
		}
		if img.Name == "" {
			t.Errorf("e2eImageSpec has empty Name: %+v", img)
		}
	}
}

func TestE2EImageSpecs_ContainsRequiredTargets(t *testing.T) {
	t.Parallel()
	required := map[string]bool{"controller": false, "agent": false, "node": false}
	for _, img := range e2eImageSpecs {
		if _, ok := required[img.Target]; ok {
			required[img.Target] = true
		}
	}
	for target, found := range required {
		if !found {
			t.Errorf("e2eImageSpecs missing required target %q", target)
		}
	}
}
