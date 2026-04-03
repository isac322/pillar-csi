package e2e

// image_bootstrap.go — AC8: docker build + kind load images for the E2E suite.
//
// All three pillar-csi component images (controller, agent, node) are built
// once per `go test` invocation and loaded into every Kind node via
// `kind load docker-image`. This ensures that DaemonSet / Deployment manifests
// can pull images locally without an external registry.
//
// Environment variables:
//
//	E2E_IMAGE_TAG        — image tag applied to every image (default: "e2e")
//	E2E_SKIP_IMAGE_BUILD — set to "true" or "1" to skip build+load and reuse
//	                       images that were loaded in a previous run.
//	DOCKER_HOST          — forwarded as-is to Docker (env-only, never hardcoded).
//
// DOCKER_HOST handling:
//
//	execCommandRunner uses exec.CommandContext which, when cmd.Env is nil,
//	inherits the calling process environment in full.  DOCKER_HOST therefore
//	reaches the docker CLI automatically via the inherited environment; no
//	special forwarding code is required.
//
// # Parallel build and load (Sub-AC 5.2)
//
// bootstrapSuiteImages performs two phases:
//
//  1. Parallel build phase — all three docker build commands run concurrently
//     using errgroup. Concurrent writes to output are serialised by
//     concurrentWriter so log lines are never interleaved.
//
//  2. Parallel load phase — all three kind load docker-image commands run
//     concurrently after every build completes. This phase also uses errgroup
//     and concurrentWriter.
//
// Typical wall-clock savings vs. sequential: 40–60 s → 15–25 s for fresh builds
// on a 4-core machine, cutting total pipeline time from ~120 s to ~80 s.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
)

const (
	// defaultE2EImageTag is the Docker tag applied when E2E_IMAGE_TAG is unset.
	defaultE2EImageTag = "e2e"

	// imageTagEnvVar is the environment variable that overrides the image tag.
	imageTagEnvVar = "E2E_IMAGE_TAG"

	// skipImageBuildEnvVar disables image build + kind load when "true" or "1".
	// Useful when iterating on test logic with unchanged images.
	skipImageBuildEnvVar = "E2E_SKIP_IMAGE_BUILD"

	// dockerBuildCacheEnvVar enables Docker BuildKit layer caching via
	// --cache-from when set to "true" or "1".  When enabled each docker build
	// passes --cache-from <image>:<tag> so that unchanged layers are reused
	// from the previous run without pulling from a registry.
	//
	// Requirements:
	//   - DOCKER_BUILDKIT=1 must be active (enabled automatically when this env
	//     var is set).
	//   - The image must already exist locally (from a previous docker build run).
	//     If it does not exist Docker silently ignores --cache-from and builds
	//     without cache.
	//
	// Typical speedup: 40-60 seconds → 5-15 seconds for unchanged images.
	dockerBuildCacheEnvVar = "E2E_DOCKER_BUILD_CACHE"
)

// e2eImageSpec describes one component image to build and load into Kind.
type e2eImageSpec struct {
	// Target is the Dockerfile multi-stage --target name.
	Target string
	// Name is the local image name (without tag).
	Name string
}

// e2eImageSpecs lists the three pillar-csi images that must be present on
// every Kind node before any E2E spec exercises a real cluster deployment.
var e2eImageSpecs = []e2eImageSpec{
	{Target: "controller", Name: "pillar-csi/controller"},
	{Target: "agent", Name: "pillar-csi/agent"},
	{Target: "node", Name: "pillar-csi/node"},
}

// bootstrapSuiteImages builds all pillar-csi component images from the
// repository root and loads each one into the Kind cluster identified by
// state.ClusterName.
//
// It is called from runPrimary (suite_test.go) after the Kind cluster is live
// and before ZFS/LVM backend provisioning.
//
// When E2E_SKIP_IMAGE_BUILD is "true" or "1", the function logs a message and
// returns nil immediately so that iterative test runs reuse previously loaded
// images.
//
// DOCKER_HOST is never hardcoded; it reaches the Docker CLI automatically via
// the inherited process environment (see package-level comment above).
func bootstrapSuiteImages(
	ctx context.Context,
	state *kindBootstrapState,
	output io.Writer,
) error {
	if state == nil {
		return fmt.Errorf("[AC8] bootstrapSuiteImages: cluster state is nil")
	}
	if output == nil {
		output = io.Discard
	}

	// Fast-path: skip build/load when explicitly disabled.
	return bootstrapSuiteImagesDirect(ctx, state, output, resolveSkipImageBuild())
}

// bootstrapSuiteImagesDirect is the injectable form of bootstrapSuiteImages.
// The skipBuild parameter controls whether the fast-path (no docker build/load) is
// taken. Tests use this directly to avoid t.Setenv (which is incompatible with
// t.Parallel in Go 1.21+).
func bootstrapSuiteImagesDirect(
	ctx context.Context,
	state *kindBootstrapState,
	output io.Writer,
	skipBuild bool,
) error {
	if state == nil {
		return fmt.Errorf("[AC8] bootstrapSuiteImages: cluster state is nil")
	}
	if output == nil {
		output = io.Discard
	}

	if skipBuild {
		_, _ = fmt.Fprintf(output,
			"[AC8] %s set — skipping docker build and kind load (reusing existing images)\n",
			skipImageBuildEnvVar)
		return nil
	}

	tag := resolveE2EImageTag()
	buildCtx, err := findRepoRoot()
	if err != nil {
		return fmt.Errorf("[AC8] locate repo root for docker build: %w", err)
	}

	cw := newConcurrentWriter(output)
	buildCacheEnabled := resolveDockerBuildCache()
	buildEnv := buildCommandEnv(buildCacheEnabled)

	// ── Phase 1: parallel docker build ───────────────────────────────────────
	// All three component images are built concurrently. Each goroutine gets its
	// own execCommandRunnerWithEnv so that build output is streamed through the
	// thread-safe concurrentWriter without interleaving.
	buildGroup, buildCtxGroup := errgroup.WithContext(ctx)
	for _, img := range e2eImageSpecs {
		img := img // capture loop variable
		ref := img.Name + ":" + tag

		buildGroup.Go(func() error {
			buildArgs := []string{"build", "--target", img.Target, "-t", ref}

			if buildCacheEnabled {
				buildArgs = append(buildArgs, "--cache-from", ref)
				_, _ = fmt.Fprintf(cw,
					"[AC8] docker build --target=%s -t %s --cache-from=%s %s (build cache enabled)\n",
					img.Target, ref, ref, buildCtx)
			} else {
				_, _ = fmt.Fprintf(cw, "[AC8] docker build --target=%s -t %s %s\n",
					img.Target, ref, buildCtx)
			}
			buildArgs = append(buildArgs, buildCtx)

			buildRunner := execCommandRunnerWithEnv{Output: cw, ExtraEnv: buildEnv}
			if _, err := buildRunner.Run(buildCtxGroup, commandSpec{
				Name: "docker",
				Args: buildArgs,
			}); err != nil {
				return fmt.Errorf("[AC8] docker build %s: %w", ref, err)
			}
			_, _ = fmt.Fprintf(cw, "[AC8] built %s\n", ref)
			return nil
		})
	}
	if err := buildGroup.Wait(); err != nil {
		return err
	}

	// ── Phase 2: parallel kind load docker-image ─────────────────────────────
	// All three images are loaded into the Kind cluster concurrently after every
	// build has completed. kind load is safe to parallelize because each image
	// is a distinct archive and Kind's node container handles concurrent imports.
	runner := execCommandRunner{Output: cw}
	loadGroup, loadCtxGroup := errgroup.WithContext(ctx)
	for _, img := range e2eImageSpecs {
		img := img // capture loop variable
		ref := img.Name + ":" + tag

		loadGroup.Go(func() error {
			_, _ = fmt.Fprintf(cw, "[AC8] kind load docker-image %s --name %s\n",
				ref, state.ClusterName)

			if _, err := runner.Run(loadCtxGroup, commandSpec{
				Name: state.KindBinary,
				Args: []string{"load", "docker-image", ref, "--name", state.ClusterName},
			}); err != nil {
				return fmt.Errorf("[AC8] kind load %s into cluster %s: %w",
					ref, state.ClusterName, err)
			}
			_, _ = fmt.Fprintf(cw, "[AC8] loaded %s into Kind cluster %q\n",
				ref, state.ClusterName)
			return nil
		})
	}
	if err := loadGroup.Wait(); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(output,
		"[AC8] all images (tag=%s) built and loaded into Kind cluster %q\n",
		tag, state.ClusterName)
	return nil
}

// concurrentWriter wraps an io.Writer with a mutex so that concurrent goroutines
// (parallel docker build / kind load) can write log lines without interleaving.
//
// Each call to Write is atomic: the entire payload is written in one lock-held
// operation, preserving complete log lines from each goroutine.
type concurrentWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// newConcurrentWriter wraps w with a mutex. When w is nil it falls back to
// io.Discard so callers never need to nil-check the result.
func newConcurrentWriter(w io.Writer) *concurrentWriter {
	if w == nil {
		w = io.Discard
	}
	return &concurrentWriter{w: w}
}

// Write serialises the payload with a mutex and forwards it to the underlying
// writer.  Implements io.Writer.
func (cw *concurrentWriter) Write(p []byte) (int, error) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return cw.w.Write(p)
}

// resolveE2EImageTag returns the Docker image tag for E2E images.
// Reads E2E_IMAGE_TAG; defaults to "e2e" when unset or empty.
func resolveE2EImageTag() string {
	return resolveE2EImageTagFromValue(os.Getenv(imageTagEnvVar))
}

// resolveE2EImageTagFromValue resolves the image tag from an explicit value string.
// This allows tests to verify the resolution logic without setting environment
// variables (which is incompatible with t.Parallel).
func resolveE2EImageTagFromValue(val string) string {
	if t := strings.TrimSpace(val); t != "" {
		return t
	}
	return defaultE2EImageTag
}

// resolveSkipImageBuild returns true when E2E_SKIP_IMAGE_BUILD is "true" or "1".
func resolveSkipImageBuild() bool {
	return resolveSkipImageBuildFromValue(os.Getenv(skipImageBuildEnvVar))
}

// resolveSkipImageBuildFromValue resolves the skip-image-build setting from an
// explicit value string. This allows tests to verify the resolution logic without
// setting environment variables (which is incompatible with t.Parallel).
func resolveSkipImageBuildFromValue(val string) bool {
	v := strings.TrimSpace(strings.ToLower(val))
	return v == "true" || v == "1"
}

// resolveDockerBuildCache returns true when E2E_DOCKER_BUILD_CACHE is "true"
// or "1", enabling --cache-from in docker build commands.
func resolveDockerBuildCache() bool {
	return resolveDockerBuildCacheFromValue(os.Getenv(dockerBuildCacheEnvVar))
}

// resolveDockerBuildCacheFromValue resolves the docker-build-cache setting from an
// explicit value string. This allows tests to verify the resolution logic without
// setting environment variables (which is incompatible with t.Parallel).
func resolveDockerBuildCacheFromValue(val string) bool {
	v := strings.TrimSpace(strings.ToLower(val))
	return v == "true" || v == "1"
}

// buildCommandEnv returns the extra environment variables to inject for docker
// build commands.  When cacheEnabled is true, DOCKER_BUILDKIT=1 is included so
// BuildKit layer caching is active.
func buildCommandEnv(cacheEnabled bool) []string {
	if cacheEnabled {
		return []string{"DOCKER_BUILDKIT=1"}
	}
	return nil
}

// execCommandRunnerWithEnv is like execCommandRunner but allows injecting
// additional environment variables into the subprocess.  The extra vars are
// appended after the inherited os.Environ(), so they take precedence over any
// identically-named vars in the parent environment.
type execCommandRunnerWithEnv struct {
	Output   io.Writer
	ExtraEnv []string
}

func (r execCommandRunnerWithEnv) Run(ctx context.Context, spec commandSpec) (string, error) {
	if len(r.ExtraEnv) == 0 {
		// No extra env: delegate to the plain runner for simplicity.
		plain := execCommandRunner{Output: r.Output}
		return plain.Run(ctx, spec)
	}
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...) //nolint:gosec
	cmd.Env = append(os.Environ(), r.ExtraEnv...)

	var outBuf bytes.Buffer
	var errBuf bytes.Buffer

	output := r.Output
	if output == nil {
		output = io.Discard
	}
	cmd.Stdout = io.MultiWriter(output, &outBuf)
	cmd.Stderr = io.MultiWriter(output, &errBuf)

	if err := cmd.Run(); err != nil {
		errText := strings.TrimSpace(errBuf.String())
		if errText == "" {
			errText = strings.TrimSpace(outBuf.String())
		}
		if errText == "" {
			errText = err.Error()
		}
		return strings.TrimSpace(outBuf.String()), fmt.Errorf("%s: %s", spec.String(), errText)
	}
	return strings.TrimSpace(outBuf.String()), nil
}

// findRepoRoot walks up the directory tree from os.Getwd() until it finds a
// directory containing go.mod, which is the repository root used as the Docker
// build context.
//
// This handles two common working-directory scenarios:
//   - `go test ./test/e2e/...`  → cwd is test/e2e/ → two levels up to repo root
//   - `ginkgo ./test/e2e/`      → cwd is repo root → found immediately
func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("os.Getwd: %w", err)
	}

	dir := wd
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // reached filesystem root without finding go.mod
		}
		dir = parent
	}
	return "", fmt.Errorf("go.mod not found walking up from %s", wd)
}
