package e2e

import (
	"context"
	"os"
	"testing"
)

// ─── resolveE2EImageTag ───────────────────────────────────────────────────────

func TestResolveE2EImageTag_Default(t *testing.T) {
	t.Setenv(imageTagEnvVar, "")
	got := resolveE2EImageTag()
	if got != defaultE2EImageTag {
		t.Errorf("resolveE2EImageTag()=%q, want %q", got, defaultE2EImageTag)
	}
}

func TestResolveE2EImageTag_EnvSet(t *testing.T) {
	t.Setenv(imageTagEnvVar, "v1.2.3")
	got := resolveE2EImageTag()
	if got != "v1.2.3" {
		t.Errorf("resolveE2EImageTag()=%q, want %q", got, "v1.2.3")
	}
}

func TestResolveE2EImageTag_Whitespace(t *testing.T) {
	t.Setenv(imageTagEnvVar, "  my-tag  ")
	got := resolveE2EImageTag()
	if got != "my-tag" {
		t.Errorf("resolveE2EImageTag()=%q, want %q", got, "my-tag")
	}
}

// ─── resolveSkipImageBuild ───────────────────────────────────────────────────

func TestResolveSkipImageBuild_False(t *testing.T) {
	t.Setenv(skipImageBuildEnvVar, "")
	if resolveSkipImageBuild() {
		t.Error("resolveSkipImageBuild()=true, want false when env is empty")
	}
}

func TestResolveSkipImageBuild_True(t *testing.T) {
	for _, val := range []string{"true", "TRUE", "True", "1"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv(skipImageBuildEnvVar, val)
			if !resolveSkipImageBuild() {
				t.Errorf("resolveSkipImageBuild()=false, want true for %q", val)
			}
		})
	}
}

func TestResolveSkipImageBuild_FalseValues(t *testing.T) {
	for _, val := range []string{"false", "0", "no", "off"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv(skipImageBuildEnvVar, val)
			if resolveSkipImageBuild() {
				t.Errorf("resolveSkipImageBuild()=true, want false for %q", val)
			}
		})
	}
}

// ─── findRepoRoot ─────────────────────────────────────────────────────────────

func TestFindRepoRoot_Success(t *testing.T) {
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
	err := bootstrapSuiteImages(context.Background(), nil, os.Stderr)
	if err == nil {
		t.Error("bootstrapSuiteImages(nil state) returned nil, want error")
	}
}

func TestBootstrapSuiteImages_SkipWhenEnvSet(t *testing.T) {
	t.Setenv(skipImageBuildEnvVar, "true")

	// Pass a non-nil but otherwise empty state — the skip path must return nil
	// before any docker/kind commands are attempted.
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

	err := bootstrapSuiteImages(context.Background(), state, os.Stderr)
	if err != nil {
		t.Errorf("bootstrapSuiteImages with skip=true returned error: %v", err)
	}
}

// ─── e2eImageSpecs sanity ─────────────────────────────────────────────────────

func TestE2EImageSpecs_NotEmpty(t *testing.T) {
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
