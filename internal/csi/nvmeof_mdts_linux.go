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
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// nvmeIoctlAdminCmd is NVME_IOCTL_ADMIN_CMD, _IOWR('N', 0x41, struct
// nvme_passthru_cmd); x/sys/unix does not define it.
const nvmeIoctlAdminCmd = 0xc0484e41

// Identify Controller command parameters (NVMe Base Specification): the
// Identify opcode, CNS 01h in CDW10, the 4 KiB data structure, and the
// offset of its MDTS byte.
const (
	nvmeAdminIdentify         = 0x06
	nvmeIdentifyCNSController = 0x01
	nvmeIdentifyDataLen       = 4096
	nvmeIDCtrlMDTSOffset      = 77
)

// nvmePassthruCmd is struct nvme_passthru_cmd of <linux/nvme_ioctl.h>
// (72 bytes; the field order gives the same layout without padding).
type nvmePassthruCmd struct {
	Opcode      uint8
	Flags       uint8
	Rsvd1       uint16
	NSID        uint32
	CDW2        uint32
	CDW3        uint32
	Metadata    uint64
	Addr        uint64
	MetadataLen uint32
	DataLen     uint32
	CDW10       uint32
	CDW11       uint32
	CDW12       uint32
	CDW13       uint32
	CDW14       uint32
	CDW15       uint32
	TimeoutMS   uint32
	Result      uint32
}

// The ioctl number encodes the 72-byte struct size; fail the build if the
// Go layout ever differs.
var _ [72]byte = [unsafe.Sizeof(nvmePassthruCmd{})]byte{}

// ReadNVMeControllerMDTS issues Identify Controller to /dev/<ctrl> and
// returns its MDTS byte.  The controller character device is created from
// sysfs first when the container's /dev lacks it (see ensureNVMeCtrlDev).
func ReadNVMeControllerMDTS(ctrl string) (uint8, error) {
	devPath, err := ensureNVMeCtrlDev(ctrl)
	if err != nil {
		return 0, err
	}
	fd, err := unix.Open(devPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", devPath, err)
	}
	defer func() { _ = unix.Close(fd) }() //nolint:errcheck // read-only device fd

	data := make([]byte, nvmeIdentifyDataLen)
	// The kernel writes into data through the address stored in cmd, which
	// the runtime cannot see; pinning keeps data on the heap and in place.
	var pinner runtime.Pinner
	pinner.Pin(&data[0])
	defer pinner.Unpin()

	cmd := nvmePassthruCmd{
		Opcode:  nvmeAdminIdentify,
		Addr:    uint64(uintptr(unsafe.Pointer(&data[0]))), //nolint:gosec // G103: kernel data buffer address
		DataLen: nvmeIdentifyDataLen,
		CDW10:   nvmeIdentifyCNSController,
	}
	status, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), nvmeIoctlAdminCmd,
		uintptr(unsafe.Pointer(&cmd))) //nolint:gosec // G103: NVME_IOCTL_ADMIN_CMD takes a struct pointer
	if errno != 0 {
		return 0, fmt.Errorf("ioctl NVME_IOCTL_ADMIN_CMD identify controller on %s: %w", devPath, errno)
	}
	// A positive return value is the NVMe completion status of a failed
	// command; the data buffer is not valid then.
	if status != 0 {
		return 0, fmt.Errorf("identify controller on %s: NVMe status %#x", devPath, status)
	}
	return data[nvmeIDCtrlMDTSOffset], nil
}
