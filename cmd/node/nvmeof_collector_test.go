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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/isac322/pillar-csi/internal/nvmeofnqn"
)

const nvmeofControllersHelp = "NVMe-oF controllers of pillar-csi subsystems on this node, by target address and state."

// writeSysfsController creates <root>/class/nvme-subsystem/<subsys> with the
// given subsysnqn and one controller directory carrying address and state.
func writeSysfsController(t *testing.T, root, subsys, nqn, ctrl, address, state string) {
	t.Helper()
	subsysPath := filepath.Join(root, "class", "nvme-subsystem", subsys)
	ctrlPath := filepath.Join(subsysPath, ctrl)
	err := os.MkdirAll(ctrlPath, 0o750)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(subsysPath, "subsysnqn"): nqn + "\n",
		filepath.Join(ctrlPath, "address"):     address + "\n",
		filepath.Join(ctrlPath, "state"):       state + "\n",
	}
	for path, content := range files {
		err = os.WriteFile(path, []byte(content), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestNVMeoFControllersCollector_PillarSubsystemsOnly verifies T8: with one
// pillar and one foreign subsystem connected, only the pillar controller is
// exported, labeled traddr:trsvcid and with its sysfs state.
func TestNVMeoFControllersCollector_PillarSubsystemsOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSysfsController(t, root, "nvme-subsys0", nvmeofnqn.Prefix+"tank.pvc-a", "nvme0",
		"traddr=192.0.2.10,trsvcid=4420,src_addr=192.0.2.99", "live")
	writeSysfsController(t, root, "nvme-subsys1", "nqn.2014-08.org.example:foreign", "nvme1",
		"traddr=198.51.100.7,trsvcid=4420", "live")

	want := `
# HELP pillar_csi_node_nvmeof_controllers ` + nvmeofControllersHelp + `
# TYPE pillar_csi_node_nvmeof_controllers gauge
pillar_csi_node_nvmeof_controllers{state="live",target_address="192.0.2.10:4420"} 1
`
	err := testutil.CollectAndCompare(newNVMeoFControllersCollector(root), strings.NewReader(want))
	if err != nil {
		t.Fatal(err)
	}
}

// TestNVMeoFControllersCollector_NoSubsystemClass verifies that a node
// without the nvme-subsystem class exports no series and no error.
func TestNVMeoFControllersCollector_NoSubsystemClass(t *testing.T) {
	t.Parallel()
	n := testutil.CollectAndCount(newNVMeoFControllersCollector(t.TempDir()))
	if n != 0 {
		t.Fatalf("series = %d, want 0", n)
	}
}

// TestNVMeoFControllersCollector_StateClosedSet verifies that an unknown
// controller state and an IPv6 target map into the documented label shapes.
func TestNVMeoFControllersCollector_StateClosedSet(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSysfsController(t, root, "nvme-subsys2", nvmeofnqn.Prefix+"tank.pvc-b", "nvme2",
		"traddr=2001:db8::1,trsvcid=4420", "frozen")

	want := `
# HELP pillar_csi_node_nvmeof_controllers ` + nvmeofControllersHelp + `
# TYPE pillar_csi_node_nvmeof_controllers gauge
pillar_csi_node_nvmeof_controllers{state="other",target_address="[2001:db8::1]:4420"} 1
`
	err := testutil.CollectAndCompare(newNVMeoFControllersCollector(root), strings.NewReader(want))
	if err != nil {
		t.Fatal(err)
	}
}
