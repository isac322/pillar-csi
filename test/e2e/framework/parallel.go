//go:build e2e
// +build e2e

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

package framework

// parallel.go — Worker-pool helpers for parallel image operations in e2e setup.
//
// # Why a semaphore?
//
// Pulling many container images simultaneously from Docker Hub (or any public
// registry) triggers HTTP 429 (rate-limited) responses when the concurrent
// connection count exceeds a per-IP threshold.  Additionally, each `docker
// pull` process opens its own TCP connections; bursting dozens of pulls at
// once saturates the CI host's upload/download bandwidth and actually slows
// the overall setup compared with a bounded pool.
//
// A buffered channel acting as a counting semaphore caps the number of active
// pulls to maxPullConcurrency=6, which empirically keeps Docker Hub happy
// while achieving good parallelism on a typical CI network connection.
//
// # errgroup usage
//
// golang.org/x/sync/errgroup provides structured concurrency: it spawns N
// goroutines, waits for all of them to finish, and returns the first non-nil
// error.  Combining errgroup with a semaphore channel is idiomatic Go for
// "fan-out with bounded concurrency and early-exit on error".

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// MaxPullConcurrency is the maximum number of simultaneous docker pull
// operations allowed by PullImages.
//
// 6 has been chosen empirically:
//   - Docker Hub allows ~10 concurrent pulls per unauthenticated IP, so 6
//     leaves headroom for retries without triggering rate-limits.
//   - Each pull process opens ~2 TCP connections; 6 pulls = 12 connections,
//     well within typical CI firewall and NIC limits.
//   - At 6 concurrent pulls a 100 Mbit/s CI link is already near saturation
//     for typical sidecar images (10–50 MB each).
//
// The constant is exported so callers can reference it in log messages.
const MaxPullConcurrency = 6

// PullImages fans out fn(ctx, image) across all images with at most
// maxPullConcurrency goroutines running concurrently.
//
// PullImages is the authoritative parallelism helper for all "pull + load"
// operations in e2e setup.  It is intentionally generic (accepts any
// func(context.Context, string) error) so it can be reused for
// pull-only, pull+load, and load-only workflows.
//
// Concurrency control: a buffered channel of capacity maxPullConcurrency acts
// as a counting semaphore.  Each goroutine sends a token before starting work
// and receives (i.e. removes) the token when it returns.  Attempting to send
// when the channel is full blocks the goroutine until a slot is available,
// naturally limiting the active worker count.
//
// Error handling: an errgroup cancels the shared context on the first error
// and returns that error after all goroutines have exited.  Callers receive a
// single, clean error rather than a channel of errors.
//
// Context propagation: if ctx is already cancelled before all images have been
// dispatched, the semaphore acquire blocks on a select that also watches
// ctx.Done(), so no goroutine is ever leaked.
//
// Example:
//
//	err := framework.PullImages(ctx, framework.ThirdPartyImages,
//	    func(ctx context.Context, img string) error {
//	        if err := exec.CommandContext(ctx, "docker", "pull", img).Run(); err != nil {
//	            return fmt.Errorf("docker pull %s: %w", img, err)
//	        }
//	        return loadImageIntoKindNodes(img)
//	    },
//	)
func PullImages(ctx context.Context, images []string, fn func(ctx context.Context, image string) error) error {
	g, gCtx := errgroup.WithContext(ctx)

	// sem is a counting semaphore: capacity = maxPullConcurrency.
	// Sending acquires a slot; receiving releases it.
	sem := make(chan struct{}, MaxPullConcurrency)

	for _, img := range images {
		img := img // capture loop variable for the closure
		g.Go(func() error {
			// Acquire a semaphore slot (or abort if context is cancelled).
			select {
			case sem <- struct{}{}:
			case <-gCtx.Done():
				return gCtx.Err()
			}
			defer func() { <-sem }() // release slot when done

			return fn(gCtx, img)
		})
	}

	return g.Wait()
}
