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
	"os"
	"os/exec"
)

// ─────────────────────────────────────────────────────────────────────────────
// Topology key constants
// ─────────────────────────────────────────────────────────────────────────────.

// Topology segment keys and values used in NodeGetInfo.AccessibleTopology.
// The CO (Container Orchestrator) uses these keys in StorageClass
// allowedTopologies to schedule volumes only on nodes that support the
// required protocol.
//
// RFC §5.8: topology keys are set based on registered protocol handlers
// detected at node plugin startup.
const (
	// TopologyKeyNVMeoF is set to topologyValueTrue when the NVMe-oF TCP
	// kernel module is loaded (/sys/module/nvme_tcp exists).
	TopologyKeyNVMeoF = "pillar-csi.bhyoo.com/nvmeof"

	// TopologyKeyISCSI is set to topologyValueTrue when the iSCSI initiator
	// is available (iscsid process running or /etc/iscsi/initiatorname.iscsi
	// exists).
	TopologyKeyISCSI = "pillar-csi.bhyoo.com/iscsi"

	// TopologyKeyNFS is set to topologyValueTrue when the mount.nfs binary
	// is present.
	TopologyKeyNFS = "pillar-csi.bhyoo.com/nfs"

	// Segment value used to mark protocols present on this node.
	// Absent protocols are omitted rather than set to "false".
	topologyValueTrue = "true"
)

// ─────────────────────────────────────────────────────────────────────────────
// ProtocolProber interface
// ─────────────────────────────────────────────────────────────────────────────.

// ProtocolProber determines which storage protocols are available on the
// current node.  A real implementation inspects kernel modules, system
// binaries, and config files.  A test implementation returns pre-programmed
// results without touching the host system.
type ProtocolProber interface {
	// NVMeoFAvailable returns true when the NVMe-oF TCP kernel module is
	// loaded and the node can act as an NVMe-oF initiator.
	NVMeoFAvailable() bool

	// ISCSIAvailable returns true when the iSCSI initiator daemon or config
	// file is present and the node can act as an iSCSI initiator.
	ISCSIAvailable() bool

	// NFSAvailable returns true when the mount.nfs binary is present and the
	// node can mount NFS volumes.
	NFSAvailable() bool
}

// ─────────────────────────────────────────────────────────────────────────────
// WithTopologyProber
// ─────────────────────────────────────────────────────────────────────────────.

// WithTopologyProber replaces the ProtocolProber used by NodeGetInfo and
// returns the receiver.  Call this during construction in tests to inject a
// mock prober without requiring kernel modules or system binaries.
//
//	srv := NewNodeServerWithStateDir(nodeID, conn, mnt, dir).WithTopologyProber(mock)
func (n *NodeServer) WithTopologyProber(p ProtocolProber) *NodeServer {
	n.topologyProber = p
	return n
}

// ─────────────────────────────────────────────────────────────────────────────
// sysfsProber — production ProtocolProber backed by sysfs / procfs
// ─────────────────────────────────────────────────────────────────────────────.

// sysfsProber is the default ProtocolProber used in production.  It checks
// kernel module state via /sys/module, iSCSI availability via procfs and the
// initiatorname config file, and NFS availability via PATH lookup.
type sysfsProber struct{}

// NVMeoFAvailable implements ProtocolProber.
// Returns true when /sys/module/nvme_tcp exists, indicating the NVMe-oF TCP
// kernel module is loaded.
func (*sysfsProber) NVMeoFAvailable() bool {
	_, err := os.Stat("/sys/module/nvme_tcp")
	return err == nil
}

// ISCSIAvailable implements ProtocolProber.
// Returns true when either:
//   - /etc/iscsi/initiatorname.iscsi exists (iSCSI initiator is configured), or
//   - iscsid binary is reachable via PATH (daemon may be running).
func (*sysfsProber) ISCSIAvailable() bool {
	_, statErr := os.Stat("/etc/iscsi/initiatorname.iscsi")
	if statErr == nil {
		return true
	}
	_, err := exec.LookPath("iscsid")
	return err == nil
}

// NFSAvailable implements ProtocolProber.
// Returns true when mount.nfs is reachable via PATH.
func (*sysfsProber) NFSAvailable() bool {
	_, err := exec.LookPath("mount.nfs")
	return err == nil
}

// ─────────────────────────────────────────────────────────────────────────────
// buildTopologySegments helper
// ─────────────────────────────────────────────────────────────────────────────.

// buildTopologySegments probes protocol availability and returns the topology
// segment map for NodeGetInfo.AccessibleTopology.  Only protocols that are
// available are included; absent protocols are omitted rather than set to
// "false" so that StorageClass selectors using In/NotIn operators work
// correctly with sparse topology maps.
func buildTopologySegments(p ProtocolProber) map[string]string {
	segs := make(map[string]string)
	if p.NVMeoFAvailable() {
		segs[TopologyKeyNVMeoF] = topologyValueTrue
	}
	if p.ISCSIAvailable() {
		segs[TopologyKeyISCSI] = topologyValueTrue
	}
	if p.NFSAvailable() {
		segs[TopologyKeyNFS] = topologyValueTrue
	}
	return segs
}
