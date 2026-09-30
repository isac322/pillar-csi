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

package lio

import (
	"slices"
	"strings"

	"github.com/google/uuid"
)

// unitSerialNamespace scopes the name-based UUIDs used as unit serials.  It
// must never change: every exported LUN's identity depends on it.
var unitSerialNamespace = uuid.MustParse("6f3a1c52-8d4e-5b7a-9c0f-2e1d4b6a8c37")

// DeriveUnitSerial returns the T10 VPD unit serial of the backstore behind
// the target named iqn.  LIO derives the LUN's NAA WWN (VPD page 0x83) from
// the unit serial, and initiators identify the disk by it, so it is a pure
// function of the IQN (itself a function of the volume ID): it is identical
// across agent restarts, storage-node reboots, local-attach round trips and
// loss of all agent state, and needs no persisted record.
func DeriveUnitSerial(iqn string) string {
	return uuid.NewSHA1(unitSerialNamespace, []byte("vpd_unit_serial:"+iqn)).String()
}

// Available reports whether the LIO iSCSI target can be used: <root>/target
// exists (target_core_mod loaded) and <root>/target/iscsi exists or can be
// created, which registers the iscsi fabric.
func Available(fsys FS, root string) error {
	return newConfigfs(fsys, root).ensureISCSIFabric()
}

// ExportedTarget is one pillar-csi-owned iSCSI target found in configfs.
type ExportedTarget struct {
	// IQN is the target name.
	IQN string
	// Portals are the network portal names of tpgt_1, sorted.
	Portals []string
	// DevicePath is the udev_path of the backstore LUN 0 is bound to; ""
	// while the target has no LUN 0 (e.g. local attach).
	DevicePath string
}

// ListTargets returns every target with OwnedIQNPrefix, sorted by IQN.  A
// missing iscsi fabric directory means no targets.
func ListTargets(fsys FS, root string) ([]ExportedTarget, error) {
	c := newConfigfs(fsys, root)
	names, err := c.subdirs(c.iscsiDir())
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	var targets []ExportedTarget
	for _, iqn := range names {
		if !strings.HasPrefix(iqn, OwnedIQNPrefix) {
			continue
		}
		t := &Target{FS: fsys, ConfigfsRoot: root, IQN: iqn}
		portals, listErr := c.subdirs(t.npDir())
		if listErr != nil {
			return nil, listErr
		}
		slices.Sort(portals)
		devicePath, devErr := t.boundDevicePath()
		if devErr != nil {
			return nil, devErr
		}
		targets = append(targets, ExportedTarget{IQN: iqn, Portals: portals, DevicePath: devicePath})
	}
	return targets, nil
}

// boundDevicePath returns the udev_path of the backstore LUN 0 links to.
func (t *Target) boundDevicePath() (string, error) {
	c := t.cfs()
	links, err := c.symlinks(t.lunDir())
	if err != nil {
		return "", err
	}
	for _, backstore := range links {
		return c.backstoreDevice(backstore)
	}
	return "", nil
}
