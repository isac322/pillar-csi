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

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	csisvc "github.com/isac322/pillar-csi/internal/csi"
)

const detachTestNQN = "nqn.2024-01.com.example:detach-vol"

// detachSysfs builds a sysfs tree with one subsystem for detachTestNQN that
// links controller nvme3, and returns the root plus that subsystem entry.
func detachSysfs(t *testing.T) (root, subsysCtrl string) {
	t.Helper()
	root = t.TempDir()
	subsys := filepath.Join(root, "class", "nvme-subsystem", "nvme-subsys3")
	subsysCtrl = filepath.Join(subsys, "nvme3")
	for _, dir := range []string{subsysCtrl, filepath.Join(root, "class", "nvme", "nvme3")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(subsys, "subsysnqn"), []byte(detachTestNQN+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The kernel attribute exists; the connector writes it without O_CREAT.
	deletePath := filepath.Join(root, "class", "nvme", "nvme3", "delete_controller")
	if err := os.WriteFile(deletePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, subsysCtrl
}

// Detach on the production connector must return only after the kernel has
// removed the controller, so an immediate re-stage of the same volume never
// connects next to a dying controller (issue #102).
func TestFabricsConnectorDetach_WaitsForControllerRemoval(t *testing.T) {
	root, subsysCtrl := detachSysfs(t)
	deletePath := filepath.Join(root, "class", "nvme", "nvme3", "delete_controller")

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if data, err := os.ReadFile(deletePath); err == nil && string(data) == "1" { //nolint:gosec
				break
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
		time.Sleep(200 * time.Millisecond) // teardown still running after the write returned
		if err := os.RemoveAll(subsysCtrl); err != nil {
			t.Errorf("remove %s: %v", subsysCtrl, err)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})

	c := &fabricsConnector{
		sysfsRoot:   root,
		removalWait: csisvc.ControllerRemovalWait{Timeout: 5 * time.Second, PollInterval: 5 * time.Millisecond},
	}
	if err := c.Detach(context.Background(), &csisvc.NVMeoFProtocolState{SubsysNQN: detachTestNQN}); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if _, err := os.Stat(subsysCtrl); !os.IsNotExist(err) {
		t.Fatalf("Detach returned before the controller left the subsystem (stat err=%v)", err)
	}
}

// A controller that outlives the bounded wait fails Detach so
// NodeUnstageVolume reports the incomplete teardown and kubelet retries.
func TestFabricsConnectorDetach_ControllerNeverRemoved_ReturnsError(t *testing.T) {
	root, _ := detachSysfs(t)

	c := &fabricsConnector{
		sysfsRoot:   root,
		removalWait: csisvc.ControllerRemovalWait{Timeout: 100 * time.Millisecond, PollInterval: 5 * time.Millisecond},
	}
	err := c.Detach(context.Background(), &csisvc.NVMeoFProtocolState{SubsysNQN: detachTestNQN})
	if !errors.Is(err, csisvc.ErrControllerNotRemoved) {
		t.Fatalf("Detach error = %v, want ErrControllerNotRemoved", err)
	}
}
