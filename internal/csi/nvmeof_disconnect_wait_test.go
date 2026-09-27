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

// Tests for the bounded controller-removal waits (issue #102).  The kernel
// returns from a delete_controller write before the controller is gone
// whenever its own deletion (ctrl_loss_tmo expiry, a DNR status) is already
// running, so Disconnect reads sysfs back until the subsystem no longer
// links the controller, and Connect waits for such a dying controller before
// creating a new one.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const waitTestNQN = "nqn.2024-01.com.example:wait-vol"

var fastRemovalWait = ControllerRemovalWait{Timeout: 5 * time.Second, PollInterval: 5 * time.Millisecond}

// subsysCtrlPath returns the subsystem's entry for ctrlName in the fixture
// built by fakeSysfsWithController.
func subsysCtrlPath(sysfsRoot, ctrlName string) string {
	return filepath.Join(sysfsRoot, "class", "nvme-subsystem", "nvme-subsys0", ctrlName)
}

// removeAfter removes path after delay, like a kernel teardown finishing in
// the background.  Cleanup waits for a callback that already started.
func removeAfter(t *testing.T, path string, delay time.Duration) {
	t.Helper()
	done := make(chan struct{})
	timer := time.AfterFunc(delay, func() {
		defer close(done)
		if err := os.RemoveAll(path); err != nil {
			t.Errorf("remove %s: %v", path, err)
		}
	})
	t.Cleanup(func() {
		if !timer.Stop() {
			<-done
		}
	})
}

// requireGone fails the test unless path no longer resolves: a call that
// waits for the controller must not return while it is still present.
func requireGone(t *testing.T, path, msg string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s: %s still present (stat err=%v)", msg, path, err)
	}
}

// simulateKernelDelete mimics the kernel teardown: once "1" has been written
// to deleteCtrlPath it waits delay, then removes subsysEntry.  The goroutine
// stops at test cleanup if no delete request ever arrives.
func simulateKernelDelete(t *testing.T, deleteCtrlPath, subsysEntry string, delay time.Duration) {
	t.Helper()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			if data, err := os.ReadFile(deleteCtrlPath); err == nil && string(data) == "1" { //nolint:gosec
				break
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
		select {
		case <-stop:
			return
		case <-time.After(delay):
		}
		if err := os.RemoveAll(subsysEntry); err != nil {
			t.Errorf("simulate kernel delete: remove %s: %v", subsysEntry, err)
		}
	})
	t.Cleanup(func() {
		close(stop)
		wg.Wait()
	})
}

// Disconnect must not return while the subsystem still links the controller:
// the kernel finishes the teardown after the delete_controller write here.
func TestDisconnect_WaitsForControllerRemoval(t *testing.T) {
	root, deleteCtrlPath := fakeSysfsWithController(t, waitTestNQN, "nvme0")
	entry := subsysCtrlPath(root, "nvme0")
	simulateKernelDelete(t, deleteCtrlPath, entry, 200*time.Millisecond)

	c := newConnector(root, "")
	c.removalWait = fastRemovalWait
	if err := c.Disconnect(context.Background(), waitTestNQN); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	requireGone(t, entry, "Disconnect returned before the controller left the subsystem")
}

// A controller that never leaves sysfs is a failed teardown: Disconnect
// reports it (so NodeUnstageVolume fails and kubelet retries) instead of
// declaring the volume detached.
func TestDisconnect_ControllerNeverRemoved_ReturnsError(t *testing.T) {
	root, _ := fakeSysfsWithController(t, waitTestNQN, "nvme0")
	if err := os.WriteFile(filepath.Join(subsysCtrlPath(root, "nvme0"), "state"),
		[]byte("deleting\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newConnector(root, "")
	c.removalWait = ControllerRemovalWait{Timeout: 100 * time.Millisecond, PollInterval: 5 * time.Millisecond}
	err := c.Disconnect(context.Background(), waitTestNQN)
	if !errors.Is(err, ErrControllerNotRemoved) {
		t.Fatalf("Disconnect error = %v, want ErrControllerNotRemoved", err)
	}
	for _, want := range []string{waitTestNQN, "nvme0", `"deleting"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

// A retried Detach can find only a dangling subsystem link: the controller
// was torn down but a lingering reference keeps the link, and its
// delete_controller attribute is gone (ENOENT).  The controller is gone, so
// Disconnect succeeds instead of failing every retry.
func TestDisconnect_DanglingControllerLink_Succeeds(t *testing.T) {
	root, deleteCtrlPath := fakeSysfsWithController(t, waitTestNQN, "nvme0")
	if err := os.RemoveAll(filepath.Dir(deleteCtrlPath)); err != nil {
		t.Fatal(err)
	}
	entry := subsysCtrlPath(root, "nvme0")
	if err := os.Remove(entry); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "devices", "gone", "nvme0"), entry); err != nil {
		t.Fatal(err)
	}

	c := newConnector(root, "")
	c.removalWait = fastRemovalWait
	if err := c.Disconnect(context.Background(), waitTestNQN); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}

// When the attribute is already gone (ENOENT) but the controller is still
// linked, its teardown is in flight: Disconnect waits for it to finish.
func TestDisconnect_DeleteAttributeGone_WaitsForInFlightTeardown(t *testing.T) {
	root, deleteCtrlPath := fakeSysfsWithController(t, waitTestNQN, "nvme0")
	if err := os.RemoveAll(filepath.Dir(deleteCtrlPath)); err != nil {
		t.Fatal(err)
	}
	entry := subsysCtrlPath(root, "nvme0")
	removeAfter(t, entry, 150*time.Millisecond)

	c := newConnector(root, "")
	c.removalWait = fastRemovalWait
	if err := c.Disconnect(context.Background(), waitTestNQN); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	requireGone(t, entry, "Disconnect returned while the teardown was still in flight")
}

// Another volume's subsystem destroyed during the scan (its subsysnqn now
// missing) is not ours and must not fail this volume's Disconnect.
func TestDisconnect_UnrelatedSubsystemVanishing_Ignored(t *testing.T) {
	root, deleteCtrlPath := fakeSysfsWithController(t, waitTestNQN, "nvme0")
	simulateKernelDelete(t, deleteCtrlPath, subsysCtrlPath(root, "nvme0"), 0)
	vanished := filepath.Join(root, "class", "nvme-subsystem", "nvme-subsys9")
	if err := os.MkdirAll(vanished, 0o750); err != nil { // no subsysnqn: read fails with ENOENT
		t.Fatal(err)
	}

	c := newConnector(root, "")
	c.removalWait = fastRemovalWait
	if err := c.Disconnect(context.Background(), waitTestNQN); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}

// A write failure other than ENOENT means no deletion was started; it is
// returned immediately rather than masked by (or waiting out) the removal
// wait.
func TestDisconnect_DeleteWriteError_ReturnedWithoutWaiting(t *testing.T) {
	root, deleteCtrlPath := fakeSysfsWithController(t, waitTestNQN, "nvme0")
	// A directory in place of the attribute makes the write fail with EISDIR.
	if err := os.Remove(deleteCtrlPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(deleteCtrlPath, 0o750); err != nil {
		t.Fatal(err)
	}

	c := newConnector(root, "")
	c.removalWait = ControllerRemovalWait{Timeout: time.Minute, PollInterval: 5 * time.Millisecond}
	start := time.Now()
	err := c.Disconnect(context.Background(), waitTestNQN)
	if err == nil || errors.Is(err, ErrControllerNotRemoved) {
		t.Fatalf("Disconnect error = %v, want the delete_controller write error", err)
	}
	if !strings.Contains(err.Error(), deleteCtrlPath) {
		t.Errorf("error %q does not name %s", err, deleteCtrlPath)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Disconnect took %s; a failed write must not wait for removal", elapsed)
	}
}

// The wait follows the caller's context so a canceled NodeUnstageVolume
// does not keep polling.
func TestDisconnect_ContextCancelledDuringWait(t *testing.T) {
	root, _ := fakeSysfsWithController(t, waitTestNQN, "nvme0")

	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	t.Cleanup(func() { timer.Stop() })

	c := newConnector(root, "")
	c.removalWait = ControllerRemovalWait{Timeout: time.Minute, PollInterval: 5 * time.Millisecond}
	err := c.Disconnect(ctx, waitTestNQN)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Disconnect error = %v, want context.Canceled", err)
	}
}

// Connect must not create a new controller while one the kernel is deleting
// is still present, or discovery can return the dying namespace.
func TestConnect_WaitsForDyingController(t *testing.T) {
	for _, state := range []string{"dead", "deleting", "deleting (no IO)"} {
		t.Run(state, func(t *testing.T) {
			root := fakeSysfs(t, waitTestNQN, false)
			addSubsysController(t, root, "nvme0", state)
			entry := subsysCtrlPath(root, "nvme0")
			removeAfter(t, entry, 150*time.Millisecond)
			fabricsDev := fakeFabricsDev(t)

			c := newConnector(root, fabricsDev)
			c.removalWait = fastRemovalWait
			if err := c.Connect(context.Background(), waitTestNQN, "192.168.1.10", "4420",
				NVMeoFConnectOptions{}); err != nil {
				t.Fatalf("Connect: %v", err)
			}
			requireGone(t, entry, "connect issued while the dying controller was still present")
			content, err := os.ReadFile(fabricsDev) //nolint:gosec
			if err != nil {
				t.Fatal(err)
			}
			if len(content) == 0 {
				t.Fatal("no fabrics connect issued after the dying controller left")
			}
		})
	}
}

// A dying controller that never leaves fails Connect without a fabrics write.
func TestConnect_DyingControllerNeverRemoved_ReturnsError(t *testing.T) {
	root := fakeSysfs(t, waitTestNQN, false)
	addSubsysController(t, root, "nvme0", "deleting")
	fabricsDev := fakeFabricsDev(t)

	c := newConnector(root, fabricsDev)
	c.removalWait = ControllerRemovalWait{Timeout: 100 * time.Millisecond, PollInterval: 5 * time.Millisecond}
	err := c.Connect(context.Background(), waitTestNQN, "192.168.1.10", "4420", NVMeoFConnectOptions{})
	if !errors.Is(err, ErrControllerNotRemoved) {
		t.Fatalf("Connect error = %v, want ErrControllerNotRemoved", err)
	}
	content, readErr := os.ReadFile(fabricsDev) //nolint:gosec
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(content) != 0 {
		t.Fatalf("fabrics connect issued next to a dying controller: %q", content)
	}
}
