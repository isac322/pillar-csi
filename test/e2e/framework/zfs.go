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

// zfs.go — ZFS pool lifecycle helpers for pillar-csi e2e tests.
//
// createLoopbackZFSPool creates a real ZFS pool on the remote Docker host by:
//
//  1. Allocating a sparse loopback image file (via `truncate -s <size> <path>`).
//  2. Attaching the image to a free loop device (via `losetup -f --show <path>`).
//  3. Creating the ZFS pool on that loop device (via `zpool create <pool> <dev>`).
//
// All three steps run via DockerHostExec.ExecOnHost so they execute in the
// host's mount namespace — the ZFS kernel module is already loaded on the
// host and `zpool` must be called there.
//
// destroyLoopbackZFSPool mirrors the creation: it exports/destroys the pool and
// detaches the loop device.  The image file itself is also removed.
//
// Typical usage in a Ginkgo BeforeSuite / AfterSuite:
//
//	var hostExec *framework.DockerHostExec
//	var loopDev  string
//
//	var _ = BeforeSuite(func() {
//	    hostExec, _ = framework.NewDockerHostExec(ctx, "tcp://10.111.0.1:2375")
//	    loopDev, _ = framework.CreateLoopbackZFSPool(ctx, hostExec,
//	        "e2e-pool",
//	        "/tmp/e2e-pool.img",
//	        "2G",
//	    )
//	})
//
//	var _ = AfterSuite(func() {
//	    _ = framework.DestroyLoopbackZFSPool(ctx, hostExec,
//	        "e2e-pool", loopDev, "/tmp/e2e-pool.img")
//	    _ = hostExec.Close()
//	})

import (
	"context"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// CreateLoopbackZFSPool
// ─────────────────────────────────────────────────────────────────────────────

// CreateLoopbackZFSPool creates a ZFS pool named poolName on the remote Docker
// host, backed by a sparse loopback image file.
//
// Parameters:
//
//	h         – privileged exec helper targeting the remote host
//	poolName  – ZFS pool name (e.g. "e2e-pool")
//	imagePath – absolute path on the remote host where the image is created
//	            (e.g. "/tmp/e2e-zfs.img").  The parent directory must exist.
//	imageSize – size string accepted by truncate(1), e.g. "2G", "512M"
//
// On success it returns the loop device path (e.g. "/dev/loop5") so the
// caller can later tear down the pool via DestroyLoopbackZFSPool.
//
// On error the function attempts a best-effort cleanup of any resources that
// were partially allocated (loop device detached, image file removed) before
// returning.
func CreateLoopbackZFSPool(
	ctx context.Context,
	h *DockerHostExec,
	poolName, imagePath, imageSize string,
) (loopDev string, err error) {
	// ── Step 1: create sparse image file ──────────────────────────────────
	//
	// `truncate -s <size> <path>` creates a sparse file of exactly <size>
	// bytes.  Sparse allocation means only the pages that are actually written
	// consume physical blocks, keeping the image lightweight for testing.
	createCmd := fmt.Sprintf("truncate -s %s %s", shellQuote(imageSize), shellQuote(imagePath))
	res, err := h.ExecOnHost(ctx, createCmd)
	if err != nil {
		return "", fmt.Errorf("zfs pool %q: create image file %q: %w", poolName, imagePath, err)
	}
	if !res.Success() {
		return "", fmt.Errorf("zfs pool %q: create image file %q: %s", poolName, imagePath, res)
	}

	// imageCreated tracks whether we need to remove the file on error.
	imageCreated := true
	defer func() {
		if err != nil && imageCreated {
			// Best-effort: remove the image file if we are bailing out.
			_ = removeImageFile(ctx, h, imagePath)
		}
	}()

	// ── Step 2: attach the image to a loop device ─────────────────────────
	//
	// `losetup -f --show <path>` atomically finds the next free loop device,
	// attaches <path> to it, and prints the device path on stdout (e.g.
	// "/dev/loop5\n").
	losetupCmd := fmt.Sprintf("losetup -f --show %s", shellQuote(imagePath))
	res, err = h.ExecOnHost(ctx, losetupCmd)
	if err != nil {
		return "", fmt.Errorf("zfs pool %q: attach loop device for %q: %w",
			poolName, imagePath, err)
	}
	if !res.Success() {
		return "", fmt.Errorf("zfs pool %q: attach loop device for %q: %s",
			poolName, imagePath, res)
	}

	loopDev = strings.TrimSpace(res.Stdout)
	if loopDev == "" {
		return "", fmt.Errorf("zfs pool %q: losetup returned empty loop device path "+
			"(stdout=%q stderr=%q)", poolName, res.Stdout, res.Stderr)
	}

	// loopAttached tracks whether we need to detach the loop device on error.
	loopAttached := true
	defer func() {
		if err != nil && loopAttached {
			// Best-effort: detach the loop device if we are bailing out.
			_ = detachLoopDevice(ctx, h, loopDev)
		}
	}()

	// ── Step 3: create the ZFS pool on the loop device ────────────────────
	//
	// `-f` forces creation even if the device has existing labels (e.g. from a
	// previous interrupted test run that left stale metadata on the image).
	zpoolCmd := fmt.Sprintf("zpool create -f %s %s",
		shellQuote(poolName), shellQuote(loopDev))
	res, err = h.ExecOnHost(ctx, zpoolCmd)
	if err != nil {
		return "", fmt.Errorf("zfs pool %q: zpool create on %s: %w", poolName, loopDev, err)
	}
	if !res.Success() {
		return "", fmt.Errorf("zfs pool %q: zpool create on %s: %s", poolName, loopDev, res)
	}

	// Pool created successfully — suppress the deferred cleanups.
	loopAttached = false
	imageCreated = false

	return loopDev, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// DestroyLoopbackZFSPool
// ─────────────────────────────────────────────────────────────────────────────

// DestroyLoopbackZFSPool tears down a pool that was created by
// CreateLoopbackZFSPool.  It performs three steps and collects errors from all
// of them so a single failure does not skip the remaining cleanup:
//
//  1. `zpool destroy <poolName>`  – export the pool and remove kernel state.
//  2. `losetup -d <loopDev>`      – detach the loop device.
//  3. `rm -f <imagePath>`         – remove the backing image file.
//
// Each step is attempted even if earlier ones fail.  All errors are joined and
// returned together so the caller sees the complete picture.
//
// Passing an empty loopDev or imagePath skips the corresponding step.
func DestroyLoopbackZFSPool(
	ctx context.Context,
	h *DockerHostExec,
	poolName, loopDev, imagePath string,
) error {
	var errs []string

	// ── Step 1: destroy the ZFS pool ──────────────────────────────────────
	if poolName != "" {
		res, err := h.ExecOnHost(ctx, fmt.Sprintf("zpool destroy %s", shellQuote(poolName)))
		if err != nil {
			errs = append(errs, fmt.Sprintf("zpool destroy %q: %v", poolName, err))
		} else if !res.Success() {
			errs = append(errs, fmt.Sprintf("zpool destroy %q: %s", poolName, res))
		}
	}

	// ── Step 2: detach the loop device ────────────────────────────────────
	if loopDev != "" {
		if err := detachLoopDevice(ctx, h, loopDev); err != nil {
			errs = append(errs, err.Error())
		}
	}

	// ── Step 3: remove the backing image file ─────────────────────────────
	if imagePath != "" {
		if err := removeImageFile(ctx, h, imagePath); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("DestroyLoopbackZFSPool: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// detachLoopDevice runs `losetup -d <dev>` on the host to detach a loop
// device.  Returns an error if the exec mechanism fails or the command exits
// non-zero.
func detachLoopDevice(ctx context.Context, h *DockerHostExec, dev string) error {
	res, err := h.ExecOnHost(ctx, fmt.Sprintf("losetup -d %s", shellQuote(dev)))
	if err != nil {
		return fmt.Errorf("losetup -d %q: %w", dev, err)
	}
	if !res.Success() {
		return fmt.Errorf("losetup -d %q: %s", dev, res)
	}
	return nil
}

// removeImageFile runs `rm -f <path>` on the host to delete a backing image
// file.  Returns an error if the exec mechanism fails or the command exits
// non-zero.
func removeImageFile(ctx context.Context, h *DockerHostExec, path string) error {
	res, err := h.ExecOnHost(ctx, fmt.Sprintf("rm -f %s", shellQuote(path)))
	if err != nil {
		return fmt.Errorf("rm -f %q: %w", path, err)
	}
	if !res.Success() {
		return fmt.Errorf("rm -f %q: %s", path, res)
	}
	return nil
}

// shellQuote wraps s in single quotes, escaping any embedded single quotes so
// the result is safe to embed in a sh -c command string.
//
// The rule: replace every ' with '\'' (end quote, literal single-quote, re-open
// quote).  Then wrap the whole thing in single quotes.
//
// Examples:
//
//	shellQuote("e2e-pool")     → 'e2e-pool'
//	shellQuote("/tmp/foo.img") → '/tmp/foo.img'
//	shellQuote("it's fine")    → 'it'\''s fine'
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
