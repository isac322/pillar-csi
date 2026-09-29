//go:build linux

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

package csi

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/isac322/pillar-csi/internal/telemetry"
)

// FormatAndMount formats the block device at source with the given
// filesystem type and extra mkfs arguments (formatOptions) when — and only
// when — blkid finds no filesystem or partition table on it, then mounts it
// at target with the given mount options.  A device that already carries a
// filesystem is never reformatted: formatOptions are ignored for it and the
// existing filesystem is checked and mounted as SafeFormatAndMount does.
//
// The format step is done here rather than by SafeFormatAndMount because
// k8s.io/utils/mount has no way to pass extra mkfs arguments.  SafeFormatAndMount
// then finds the new filesystem and only checks and mounts it.
func (m *KubeMounter) FormatAndMount(
	ctx context.Context, source, target, fsType string, options, formatOptions []string,
) error {
	if fsType == "" {
		fsType = defaultFsType
	}
	err := m.formatIfBlank(ctx, source, fsType, options, formatOptions)
	if err != nil {
		return err
	}
	err = m.inner.FormatAndMount(source, target, fsType, options)
	if err != nil {
		return fmt.Errorf("FormatAndMount %s → %s: %w", source, target, err)
	}
	return nil
}

// formatIfBlank runs mkfs.<fsType> on source when blkid finds no filesystem
// or partition table on it.  A device that already carries data is left
// untouched, and a read-only mount request leaves a blank device alone so
// SafeFormatAndMount reports it as an unformatted read-only disk.
//
// The mkfs binary is executed directly (no shell) with options that passed
// the validateMkfsOptions allowlist.  Afterwards the device must carry a
// fsType filesystem: otherwise SafeFormatAndMount would find it still blank
// and format it again without the configured options.
//
// It sets pillar_csi.fs.detected and pillar_csi.mkfs.performed on the span in
// ctx (SP13); the mkfs run itself is observed as an SP8 child and in M9.
func (m *KubeMounter) formatIfBlank(
	ctx context.Context, source, fsType string, mountOptions, formatOptions []string,
) error {
	err := validateMkfsOptions(fsType, formatOptions)
	if err != nil {
		return fmt.Errorf("format %s as %s: %w", source, fsType, err)
	}
	existing, err := m.inner.GetDiskFormat(source)
	if err != nil {
		return fmt.Errorf("detect existing filesystem on %s: %w", source, err)
	}
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(telemetry.KeyFSDetected.String(existing))
	if existing != "" || slices.Contains(mountOptions, "ro") {
		span.SetAttributes(telemetry.KeyMkfsPerformed.Bool(false))
		return nil
	}

	args, err := mkfsArgs(fsType, source, formatOptions, m.xfsProfile)
	if err != nil {
		return fmt.Errorf("format %s: %w", source, err)
	}
	mkfs := "mkfs." + fsType
	span.SetAttributes(telemetry.KeyMkfsPerformed.Bool(true))
	obs := telemetry.StartExec(ctx, mkfs, args...)
	out, err := m.inner.Exec.CommandContext(ctx, mkfs, args...).CombinedOutput()
	obs.End(out, err)
	if err != nil {
		return fmt.Errorf("format %s: mkfs.%s %q: %w: %s",
			source, fsType, args, err, strings.TrimSpace(string(out)))
	}
	formatted, err := m.inner.GetDiskFormat(source)
	if err != nil {
		return fmt.Errorf("verify filesystem on %s after mkfs.%s: %w", source, fsType, err)
	}
	if formatted != fsType {
		return fmt.Errorf("format %s: mkfs.%s %q succeeded but blkid reports filesystem %q, want %q: %s",
			source, fsType, args, formatted, fsType, strings.TrimSpace(string(out)))
	}
	return nil
}

// mkfsArgs builds the mkfs.<fsType> argument vector.  The ext4 defaults match
// k8s.io/utils/mount.SafeFormatAndMount (force, no reserved blocks); the
// configured options follow them so a user value for the same option (e.g.
// "-m", "1") takes precedence, and the device comes last.  For xfs the
// defaults are the options of the xfsProfile configuration file (see
// xfsCompatProfile) that the configured options do not set.  The type is
// ext4 or xfs (see stageFilesystem).
func mkfsArgs(fsType, source string, formatOptions []string, xfsProfile string) ([]string, error) {
	var defaults []string
	switch fsType {
	case defaultFsType:
		defaults = []string{"-F", "-m0"}
	case xfsFsType:
		profile, err := os.ReadFile(xfsProfile) //nolint:gosec // G304: KubeMounter's fixed xfsCompatProfile path.
		if err != nil {
			return nil, fmt.Errorf("read the mkfs.xfs compatibility profile: %w", err)
		}
		defaults, err = xfsProfileArgs(string(profile), formatOptions)
		if err != nil {
			return nil, fmt.Errorf("mkfs.xfs compatibility profile %s: %w", xfsProfile, err)
		}
	}
	args := make([]string, 0, len(defaults)+len(formatOptions)+1)
	args = append(args, defaults...)
	args = append(args, formatOptions...)
	return append(args, source), nil
}
