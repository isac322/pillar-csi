package v1alpha1

import "testing"

func TestCompatible(t *testing.T) {
	t.Parallel()

	nvme := ProtocolSpec{NVMeOFTCP: &NVMeOFTCPConfig{}}
	tests := []struct {
		name        string
		backend     BackendSpec
		protocol    ProtocolSpec
		wantOK      bool
		wantMessage string
	}{
		{
			name:        "zfs over nvmeofTcp",
			backend:     BackendSpec{ZFS: &ZFSBackendConfig{Pool: "tank"}},
			protocol:    nvme,
			wantOK:      true,
			wantMessage: `backend "zfs-zvol" and protocol "nvmeof-tcp" are compatible`,
		},
		{
			name:        "lvm over nvmeofTcp",
			backend:     BackendSpec{LVM: &LVMBackendConfig{VolumeGroup: "vg"}},
			protocol:    nvme,
			wantOK:      true,
			wantMessage: `backend "lvm-lv" and protocol "nvmeof-tcp" are compatible`,
		},
		{
			name:        "zfs over iscsi",
			backend:     BackendSpec{ZFS: &ZFSBackendConfig{Pool: "tank"}},
			protocol:    ProtocolSpec{ISCSI: &ISCSIConfig{}},
			wantOK:      true,
			wantMessage: `backend "zfs-zvol" and protocol "iscsi" are compatible`,
		},
		{
			name:        "lvm over iscsi",
			backend:     BackendSpec{LVM: &LVMBackendConfig{VolumeGroup: "vg"}},
			protocol:    ProtocolSpec{ISCSI: &ISCSIConfig{}},
			wantOK:      true,
			wantMessage: `backend "lvm-lv" and protocol "iscsi" are compatible`,
		},
		{
			name:        "empty backend union",
			backend:     BackendSpec{},
			protocol:    nvme,
			wantMessage: `backend "" is incompatible with protocol "nvmeof-tcp": backend or protocol is not supported`,
		},
		{
			name:        "empty protocol union",
			backend:     BackendSpec{ZFS: &ZFSBackendConfig{Pool: "tank"}},
			protocol:    ProtocolSpec{},
			wantMessage: `backend "zfs-zvol" is incompatible with protocol "": backend or protocol is not supported`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Compatible(tt.backend, tt.protocol)
			if got.OK != tt.wantOK || got.Message != tt.wantMessage {
				t.Fatalf("Compatible() = {OK:%v Message:%q}, want {OK:%v Message:%q}",
					got.OK, got.Message, tt.wantOK, tt.wantMessage)
			}
		})
	}
}
