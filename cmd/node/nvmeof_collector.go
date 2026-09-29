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
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"

	csisvc "github.com/isac322/pillar-csi/internal/csi"
	"github.com/isac322/pillar-csi/internal/nvmeofnqn"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

// nvmeControllerStates is the closed state label set of M17: the values of
// the kernel's nvme controller sysfs state attribute.
var nvmeControllerStates = map[string]struct{}{
	"live":             {},
	"connecting":       {},
	"resetting":        {},
	"deleting":         {},
	"deleting (no IO)": {},
	"new":              {},
	"dead":             {},
}

// nvmeofControllersCollector is M17, pillar_csi_node_nvmeof_controllers: the
// NVMe-oF controllers of pillar subsystems (subsysnqn starting with
// nvmeofnqn.Prefix) connected on this node, counted by target address and
// controller state.  It walks <sysfsRoot>/class/nvme-subsystem on every
// scrape.
type nvmeofControllersCollector struct {
	sysfsRoot string
	desc      *prometheus.Desc
}

func newNVMeoFControllersCollector(sysfsRoot string) *nvmeofControllersCollector {
	return &nvmeofControllersCollector{
		sysfsRoot: sysfsRoot,
		desc: prometheus.NewDesc(
			"pillar_csi_node_nvmeof_controllers",
			"NVMe-oF controllers of pillar-csi subsystems on this node, by target address and state.",
			[]string{"target_address", "state"}, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *nvmeofControllersCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect implements prometheus.Collector.  A sysfs read failure other than
// an entry vanishing mid-scan is reported as an invalid metric, so the
// scrape handler logs it.
func (c *nvmeofControllersCollector) Collect(ch chan<- prometheus.Metric) {
	counts, err := c.scan()
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.desc, err)
	}
	for k, n := range counts {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), k.targetAddress, k.state)
	}
}

type controllerKey struct {
	targetAddress string
	state         string
}

// scan counts the controllers of every pillar subsystem.  Entries that
// vanish while being read (a teardown racing the scrape) are skipped; other
// read errors are joined and returned with the counts gathered so far.
func (c *nvmeofControllersCollector) scan() (map[controllerKey]int, error) {
	counts := make(map[controllerKey]int)
	subsysDir := filepath.Join(c.sysfsRoot, "class", "nvme-subsystem")
	entries, err := os.ReadDir(subsysDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return counts, nil // no nvme-subsystem class: nothing connected
		}
		return counts, fmt.Errorf("nvmeof controllers metric: read %s: %w", subsysDir, err)
	}

	var errs []error
	for _, entry := range entries {
		subsysPath := filepath.Join(subsysDir, entry.Name())
		nqn, readErr := readSysfsValue(filepath.Join(subsysPath, "subsysnqn"))
		if readErr != nil {
			if !vanished(readErr) {
				errs = append(errs, readErr)
			}
			continue
		}
		if !strings.HasPrefix(nqn, nvmeofnqn.Prefix) {
			continue
		}
		scanErr := scanSubsystemControllers(subsysPath, counts)
		if scanErr != nil {
			errs = append(errs, scanErr)
		}
	}
	if len(errs) > 0 {
		return counts, fmt.Errorf("nvmeof controllers metric: %w", errors.Join(errs...))
	}
	return counts, nil
}

// scanSubsystemControllers adds the controllers linked from subsysPath to
// counts.
func scanSubsystemControllers(subsysPath string, counts map[controllerKey]int) error {
	ctrlEntries, err := os.ReadDir(subsysPath)
	if err != nil {
		if vanished(err) {
			return nil
		}
		return fmt.Errorf("read subsystem dir %s: %w", subsysPath, err)
	}
	var errs []error
	for _, ctrlEntry := range ctrlEntries {
		if !csisvc.IsNVMeControllerEntry(ctrlEntry.Name()) {
			continue
		}
		ctrlPath := filepath.Join(subsysPath, ctrlEntry.Name())
		address, addrErr := readSysfsValue(filepath.Join(ctrlPath, "address"))
		if addrErr != nil {
			if !vanished(addrErr) {
				errs = append(errs, addrErr)
			}
			continue
		}
		state, stateErr := readSysfsValue(filepath.Join(ctrlPath, "state"))
		switch {
		case stateErr == nil:
			state = controllerStateLabel(state)
		case vanished(stateErr):
			// Absent on some kernels, or removed after the address read.
			state = telemetry.LabelOther
		default:
			errs = append(errs, stateErr)
			continue
		}
		counts[controllerKey{targetAddress: targetAddressLabel(address), state: state}]++
	}
	return errors.Join(errs...)
}

// controllerStateLabel maps a controller sysfs state to the M17 state label.
func controllerStateLabel(state string) string {
	if _, ok := nvmeControllerStates[state]; ok {
		return state
	}
	return telemetry.LabelOther
}

// targetAddressLabel returns "traddr:trsvcid" (IPv6 in brackets) from a
// controller's sysfs address attribute, e.g.
// "traddr=192.0.2.1,trsvcid=4420,src_addr=192.0.2.9".  An address without
// traddr maps to "other".
func targetAddressLabel(address string) string {
	var trAddr, trSvcID string
	for field := range strings.SplitSeq(address, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			continue
		}
		switch key {
		case "traddr":
			trAddr = value
		case "trsvcid":
			trSvcID = value
		}
	}
	if trAddr == "" {
		return telemetry.LabelOther
	}
	if trSvcID == "" {
		return trAddr
	}
	return net.JoinHostPort(trAddr, trSvcID)
}

// readSysfsValue reads a sysfs attribute and trims surrounding whitespace.
func readSysfsValue(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: sysfs path under the collector's fixed root.
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// vanished reports whether err means the sysfs entry disappeared during the
// scan (a controller or subsystem torn down concurrently).
func vanished(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENODEV)
}
