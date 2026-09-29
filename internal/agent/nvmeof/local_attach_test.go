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

package nvmeof

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaimDeviceExclusively_UnheldAndMissing(t *testing.T) {
	t.Parallel()
	free := filepath.Join(t.TempDir(), "dev")
	if err := os.WriteFile(free, nil, 0o600); err != nil {
		t.Fatalf("create device stand-in: %v", err)
	}
	for _, path := range []string{free, filepath.Join(t.TempDir(), "absent")} {
		release, err := ClaimDeviceExclusively(path)
		if err != nil {
			t.Errorf("ClaimDeviceExclusively(%q) = %v; want nil error", path, err)
			continue
		}
		releaseErr := release()
		if releaseErr != nil {
			t.Errorf("ClaimDeviceExclusively(%q) release = %v; want nil", path, releaseErr)
		}
	}
}

// mustRead returns the trimmed content of path, failing the test on error.
func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // G304: test helper reads from t.TempDir() paths only.
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return strings.TrimSpace(string(data))
}

func localAttachTestTarget(t *testing.T, claimer DeviceClaimer) *NvmetTarget {
	t.Helper()
	return &NvmetTarget{
		ConfigfsRoot:  t.TempDir(),
		SubsystemNQN:  "nqn.2026-01.io.pillar-csi:pvc-local",
		NamespaceID:   1,
		DevicePath:    "/dev/zvol/tank/pvc-local",
		BindAddress:   "10.0.0.1",
		Port:          DefaultPort,
		DeviceClaimer: claimer,
	}
}

// TestApply_LocalAttachLinksDisabledNamespace: a locally attached export is
// linked with its namespace disabled, and a later non-local Apply enables it.
func TestApply_LocalAttachLinksDisabledNamespace(t *testing.T) {
	t.Parallel()
	tgt := localAttachTestTarget(t, UnclaimedDeviceClaimer)
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	tgt.LocalAttach = true
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply local: %v", err)
	}
	assertFileContent(t, tgt.namespaceEnablePath(), "0")

	tgt.LocalAttach = false
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply remote: %v", err)
	}
	assertFileContent(t, tgt.namespaceEnablePath(), "1")
}

// TestPrepare_RefusesToEnableHeldDevice: Prepare keeps a disabled namespace
// disabled and reports ErrDeviceHeld while the device is claimed elsewhere,
// and a claim failure is never treated as "claimed".
func TestPrepare_RefusesToEnableHeldDevice(t *testing.T) {
	t.Parallel()
	claimErr := errors.New("permission denied")
	for name, claimer := range map[string]DeviceClaimer{
		"held":        func(string) (func() error, error) { return nil, ErrDeviceHeld },
		"claim error": func(string) (func() error, error) { return nil, claimErr },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tgt := localAttachTestTarget(t, claimer)
			tgt.LocalAttach = true
			if _, err := tgt.Prepare(); err != nil {
				t.Fatalf("Prepare local: %v", err)
			}
			tgt.LocalAttach = false
			_, err := tgt.Prepare()
			if err == nil {
				t.Fatal("Prepare enabled a namespace whose backend claim did not succeed")
			}
			if name == "held" && !errors.Is(err, ErrDeviceHeld) {
				t.Errorf("Prepare error = %v, want ErrDeviceHeld", err)
			}
			if name == "claim error" && !errors.Is(err, claimErr) {
				t.Errorf("Prepare error = %v, want the claim error", err)
			}
			assertFileContent(t, tgt.namespaceEnablePath(), "0")
		})
	}
}

// TestLink_VerifiesDesiredEnable: Link refuses a namespace whose enable state
// differs from the desired one in either direction.
func TestLink_VerifiesDesiredEnable(t *testing.T) {
	t.Parallel()
	for _, local := range []bool{true, false} {
		tgt := localAttachTestTarget(t, UnclaimedDeviceClaimer)
		tgt.LocalAttach = local
		prepared, err := tgt.Prepare()
		if err != nil {
			t.Fatalf("Prepare local=%t: %v", local, err)
		}
		wrong := "0"
		if local {
			wrong = "1"
		}
		mustWrite(t, tgt.namespaceEnablePath(), wrong)
		if err := prepared.Link(); err == nil {
			t.Errorf("Link local=%t accepted enable=%s", local, wrong)
		}
	}
}

func TestResizeNamespace_SkipsDisabledNamespace(t *testing.T) {
	t.Parallel()
	tgt := localAttachTestTarget(t, nil)
	nsDir := tgt.namespaceDir()
	if err := os.MkdirAll(filepath.Join(nsDir, "revalidate_size"), 0o750); err != nil {
		t.Fatalf("create unwritable revalidate_size: %v", err)
	}
	mustWrite(t, tgt.namespaceEnablePath(), "0")
	if err := tgt.ResizeNamespace(); err != nil {
		t.Fatalf("ResizeNamespace on disabled namespace: %v", err)
	}
}

// TestEnableNamespace_ClaimHeldAcrossEnable: the exclusive claim on the
// backend device is acquired before the enable write and released only after
// it — closing the window in which the storage node could claim the device
// under a still-disabled namespace.
func TestEnableNamespace_ClaimHeldAcrossEnable(t *testing.T) {
	t.Parallel()
	var tgt *NvmetTarget
	var events []string
	var enableAtRelease string
	tgt = localAttachTestTarget(t, func(path string) (func() error, error) {
		events = append(events, "acquire "+path)
		enableAtAcquire := mustRead(t, tgt.namespaceEnablePath())
		if enableAtAcquire != "0" {
			t.Errorf("enable at claim acquire = %q, want 0 (still fenced)", enableAtAcquire)
		}
		return func() error {
			enableAtRelease = mustRead(t, tgt.namespaceEnablePath())
			events = append(events, "release")
			return nil
		}, nil
	})
	tgt.LocalAttach = true
	if _, err := tgt.Prepare(); err != nil {
		t.Fatalf("Prepare local: %v", err)
	}
	tgt.LocalAttach = false

	if err := tgt.EnableNamespace(); err != nil {
		t.Fatalf("EnableNamespace: %v", err)
	}
	if enableAtRelease != "1" {
		t.Errorf("enable at claim release = %q, want 1 (claim held across the write)", enableAtRelease)
	}
	want := []string{"acquire " + tgt.DevicePath, "release"}
	if len(events) != len(want) || events[0] != want[0] || events[1] != want[1] {
		t.Errorf("claim events = %v, want %v", events, want)
	}
	assertFileContent(t, tgt.namespaceEnablePath(), "1")
}

// TestEnableNamespace_ReleaseErrorPropagates: a claim release failure is
// reported while the namespace state stays as written.
func TestEnableNamespace_ReleaseErrorPropagates(t *testing.T) {
	t.Parallel()
	releaseErr := errors.New("close failed")
	tgt := localAttachTestTarget(t, func(string) (func() error, error) {
		return func() error { return fmt.Errorf("release backend: %w", releaseErr) }, nil
	})
	tgt.LocalAttach = true
	if _, err := tgt.Prepare(); err != nil {
		t.Fatalf("Prepare local: %v", err)
	}
	tgt.LocalAttach = false

	err := tgt.EnableNamespace()
	if !errors.Is(err, releaseErr) {
		t.Fatalf("EnableNamespace = %v, want the release error", err)
	}
	assertFileContent(t, tgt.namespaceEnablePath(), "1")
}

// TestEnableNamespace_EmptyDevicePathSkipsClaim: a namespace whose
// device_path names no device is enabled without consulting the claimer.
func TestEnableNamespace_EmptyDevicePathSkipsClaim(t *testing.T) {
	t.Parallel()
	tgt := localAttachTestTarget(t, func(string) (func() error, error) {
		t.Error("DeviceClaimer called for a namespace without a device_path")
		return noRelease, nil
	})
	tgt.LocalAttach = true
	if _, err := tgt.Prepare(); err != nil {
		t.Fatalf("Prepare local: %v", err)
	}
	devPathAttr := filepath.Join(tgt.namespaceDir(), "device_path")
	if err := os.Remove(devPathAttr); err != nil {
		t.Fatalf("remove device_path: %v", err)
	}

	if err := tgt.EnableNamespace(); err != nil {
		t.Fatalf("EnableNamespace: %v", err)
	}
	assertFileContent(t, tgt.namespaceEnablePath(), "1")
}
