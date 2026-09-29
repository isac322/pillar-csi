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
	"os"
	"path/filepath"
	"testing"
)

func TestDeviceHeldExclusively_UnheldAndMissing(t *testing.T) {
	t.Parallel()
	free := filepath.Join(t.TempDir(), "dev")
	if err := os.WriteFile(free, nil, 0o600); err != nil {
		t.Fatalf("create device stand-in: %v", err)
	}
	for _, path := range []string{free, filepath.Join(t.TempDir(), "absent")} {
		held, err := DeviceHeldExclusively(path)
		if err != nil || held {
			t.Errorf("DeviceHeldExclusively(%q) = %t, %v; want false, nil", path, held, err)
		}
	}
}

func localAttachTestTarget(t *testing.T, probe DeviceHeldProbe) *NvmetTarget {
	t.Helper()
	return &NvmetTarget{
		ConfigfsRoot:    t.TempDir(),
		SubsystemNQN:    "nqn.2026-01.io.pillar-csi:pvc-local",
		NamespaceID:     1,
		DevicePath:      "/dev/zvol/tank/pvc-local",
		BindAddress:     "10.0.0.1",
		Port:            DefaultPort,
		DeviceHeldProbe: probe,
	}
}

// TestApply_LocalAttachLinksDisabledNamespace: a locally attached export is
// linked with its namespace disabled, and a later non-local Apply enables it.
func TestApply_LocalAttachLinksDisabledNamespace(t *testing.T) {
	t.Parallel()
	tgt := localAttachTestTarget(t, func(string) (bool, error) { return false, nil })
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
// disabled and reports ErrDeviceHeld while the device is held, and a probe
// failure is never treated as "not held".
func TestPrepare_RefusesToEnableHeldDevice(t *testing.T) {
	t.Parallel()
	probeErr := errors.New("permission denied")
	for name, probe := range map[string]DeviceHeldProbe{
		"held":        func(string) (bool, error) { return true, nil },
		"probe error": func(string) (bool, error) { return false, probeErr },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tgt := localAttachTestTarget(t, probe)
			tgt.LocalAttach = true
			if _, err := tgt.Prepare(); err != nil {
				t.Fatalf("Prepare local: %v", err)
			}
			tgt.LocalAttach = false
			_, err := tgt.Prepare()
			if err == nil {
				t.Fatal("Prepare enabled a namespace whose holder check did not pass")
			}
			if name == "held" && !errors.Is(err, ErrDeviceHeld) {
				t.Errorf("Prepare error = %v, want ErrDeviceHeld", err)
			}
			if name == "probe error" && !errors.Is(err, probeErr) {
				t.Errorf("Prepare error = %v, want the probe error", err)
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
		tgt := localAttachTestTarget(t, func(string) (bool, error) { return false, nil })
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

// TestEnableNamespace_ReprobeAfterEnable: a claim established between the
// pre-enable probe and the enable write is caught by the post-enable probe;
// the namespace is left disabled (fail closed) and the refusal is reported.
func TestEnableNamespace_ReprobeAfterEnable(t *testing.T) {
	t.Parallel()
	secondErr := errors.New("probe unavailable")
	for name, tt := range map[string]struct {
		secondHeld bool
		secondErr  error
		wantErr    error
	}{
		"held after enable":        {secondHeld: true, wantErr: ErrDeviceHeld},
		"probe error after enable": {secondErr: secondErr, wantErr: secondErr},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var calls int
			tgt := localAttachTestTarget(t, func(string) (bool, error) {
				calls++
				if calls == 1 {
					return false, nil
				}
				return tt.secondHeld, tt.secondErr
			})
			tgt.LocalAttach = true
			if _, err := tgt.Prepare(); err != nil {
				t.Fatalf("Prepare local: %v", err)
			}
			tgt.LocalAttach = false

			err := tgt.EnableNamespace()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("EnableNamespace = %v, want %v", err, tt.wantErr)
			}
			if calls != 2 {
				t.Errorf("probe calls = %d, want 2", calls)
			}
			assertFileContent(t, tgt.namespaceEnablePath(), "0")
		})
	}
}

// TestEnableNamespace_ProbesAgainAfterEnable: a clean enable still re-probes
// the backend device after the enable write and stays enabled while the
// device is unheld.
func TestEnableNamespace_ProbesAgainAfterEnable(t *testing.T) {
	t.Parallel()
	var calls int
	tgt := localAttachTestTarget(t, func(string) (bool, error) {
		calls++
		return false, nil
	})
	tgt.LocalAttach = true
	if _, err := tgt.Prepare(); err != nil {
		t.Fatalf("Prepare local: %v", err)
	}
	tgt.LocalAttach = false

	if err := tgt.EnableNamespace(); err != nil {
		t.Fatalf("EnableNamespace: %v", err)
	}
	if calls != 2 {
		t.Errorf("probe calls = %d, want 2", calls)
	}
	assertFileContent(t, tgt.namespaceEnablePath(), "1")
}
