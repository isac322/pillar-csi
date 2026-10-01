package e2e

// image_bootstrap.go — AC8: image build or exact-artifact load + Kind load.
//
// All three pillar-csi component images (controller, agent, node) are loaded
// into every Kind node via `kind load docker-image`. In the normal path they
// are built once per `go test` invocation. CI behavior lanes can instead set
// E2E_PREBUILT_IMAGES after loading an exact-checkout artifact into the local
// Docker daemon; the Kind load phase still runs for the fresh cluster.
//
// Environment variables:
//
//	E2E_IMAGE_TAG        — image tag applied to every image (default: "e2e")
//	E2E_SKIP_IMAGE_BUILD — set to "true" or "1" to skip build+load and reuse
//	                       images already loaded in an existing Kind cluster.
//	E2E_PREBUILT_IMAGES  — set to "true" or "1" when exact-checkout images are
//	                       present in Docker and must be loaded into Kind.
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
// bootstrapSuiteImages performs either:
//
//  1. Parallel local build followed by parallel Kind load; or
//  2. Prebuilt-image verification followed by parallel Kind load.
//
// In both modes image loading completes before backend provisioning, preserving
// the AC8 phase ordering and pullPolicy=Never deployment contract.

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
	// prebuiltImagesEnvVar tells the harness that CI has already loaded exact-
	// checkout images into the runner Docker daemon. The harness still performs
	// kind load after creating the fresh cluster.
	prebuiltImagesEnvVar = "E2E_PREBUILT_IMAGES"
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
// When E2E_PREBUILT_IMAGES is true, CI has already loaded an exact-checkout
// image artifact into the Docker daemon. In that mode this function verifies
// the three expected local image references and only performs the required
// kind load phase. This is intentionally distinct from E2E_SKIP_IMAGE_BUILD:
// the latter assumes images are already present in the existing Kind nodes,
// while the prebuilt mode supports a newly-created cluster.
//
// When E2E_SKIP_IMAGE_BUILD is true and no prebuilt images are configured, the
// historical iterative-development fast path is preserved unchanged.
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

	return bootstrapSuiteImagesDirectWithPrebuilt(
		ctx,
		state,
		output,
		resolveSkipImageBuild(),
		resolvePrebuiltImages(),
	)
}

// bootstrapSuiteImagesDirect is the injectable form used by unit tests. The
// prebuilt-image path is disabled so existing skip/build tests continue to
// exercise the local-build contract directly.
func bootstrapSuiteImagesDirect(
	ctx context.Context,
	state *kindBootstrapState,
	output io.Writer,
	skipBuild bool,
) error {
	return bootstrapSuiteImagesDirectWithPrebuilt(ctx, state, output, skipBuild, false)
}

func bootstrapSuiteImagesDirectWithPrebuilt(
	ctx context.Context,
	state *kindBootstrapState,
	output io.Writer,
	skipBuild bool,
	prebuiltImages bool,
) error {
	if state == nil {
		return fmt.Errorf("[AC8] bootstrapSuiteImages: cluster state is nil")
	}
	if output == nil {
		output = io.Discard
	}

	tag := resolveE2EImageTag()
	if prebuiltImages {
		_, _ = fmt.Fprintf(output,
			"[AC8] %s set — verifying and loading prebuilt images (tag=%s)\n",
			prebuiltImagesEnvVar, tag)
		if err := verifyPrebuiltImages(ctx, output, tag); err != nil {
			return err
		}
		return loadImagesIntoKind(ctx, state, output, tag)
	}

	if skipBuild {
		_, _ = fmt.Fprintf(output,
			"[AC8] %s set — skipping docker build and kind load (reusing existing images)\n",
			skipImageBuildEnvVar)
		return nil
	}

	buildCtx, err := findRepoRoot()
	if err != nil {
		return fmt.Errorf("[AC8] locate repo root for docker build: %w", err)
	}

	cw := newConcurrentWriter(output)
	buildCacheEnabled := resolveDockerBuildCache()
	buildEnv := buildCommandEnv(buildCacheEnabled)

	// ── Phase 1: parallel docker build ───────────────────────────────────────
	buildGroup, buildCtxGroup := errgroup.WithContext(ctx)
	for _, img := range e2eImageSpecs {
		img := img
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

	// ── Phase 2: parallel kind load ──────────────────────────────────────────
	return loadImagesIntoKind(ctx, state, output, tag)
}

func verifyPrebuiltImages(ctx context.Context, output io.Writer, tag string) error {
	runner := execCommandRunner{Output: output}
	for _, img := range e2eImageSpecs {
		ref := img.Name + ":" + tag
		inspectOutput, err := runner.Run(ctx, commandSpec{
			Name: "docker",
			Args: []string{"image", "inspect", "--format", "{{.Id}}", ref},
		})
		if err != nil {
			return fmt.Errorf("[AC8] prebuilt image %s unavailable: %w", ref, err)
		}
		if strings.TrimSpace(inspectOutput) == "" {
			return fmt.Errorf("[AC8] prebuilt image %s has no image ID", ref)
		}
	}
	return nil
}

func loadImagesIntoKind(
	ctx context.Context,
	state *kindBootstrapState,
	output io.Writer,
	tag string,
) error {
	if state == nil {
		return fmt.Errorf("[AC8] loadImagesIntoKind: cluster state is nil")
	}
	if output == nil {
		output = io.Discard
	}

	cw := newConcurrentWriter(output)
	runner := execCommandRunner{Output: cw}
	loadGroup, loadCtxGroup := errgroup.WithContext(ctx)
	for _, img := range e2eImageSpecs {
		img := img
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
		"[AC8] all images (tag=%s) loaded into Kind cluster %q\n",
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

// resolvePrebuiltImages returns true when CI has loaded the exact-checkout
// runtime image artifact into the local Docker daemon.
func resolvePrebuiltImages() bool {
	return resolvePrebuiltImagesFromValue(os.Getenv(prebuiltImagesEnvVar))
}

// resolvePrebuiltImagesFromValue resolves the prebuilt-image setting without
// reading process state, allowing parallel unit tests to exercise it safely.
func resolvePrebuiltImagesFromValue(val string) bool {
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
