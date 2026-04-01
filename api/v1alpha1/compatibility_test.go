package v1alpha1

import "testing"

func TestCategoryOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		backend BackendType
		want    BackendCategory
	}{
		{name: "zfs zvol is block", backend: BackendTypeZFSZvol, want: BackendCategoryBlock},
		{name: "lvm lv is block", backend: BackendTypeLVMLV, want: BackendCategoryBlock},
		{name: "zfs dataset is filesystem", backend: BackendTypeZFSDataset, want: BackendCategoryFilesystem},
		{name: "dir is filesystem", backend: BackendTypeDir, want: BackendCategoryFilesystem},
		{name: "unknown backend is unknown", backend: BackendType("mystery"), want: BackendCategoryUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := CategoryOf(tt.backend)
			if got != tt.want {
				t.Fatalf("CategoryOf(%q) = %q, want %q", tt.backend, got, tt.want)
			}
		})
	}
}

func TestProtocolCategoryOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol ProtocolType
		want     ProtocolCategory
	}{
		{name: "nvmeof tcp is block", protocol: ProtocolTypeNVMeOFTCP, want: ProtocolCategoryBlock},
		{name: "iscsi is block", protocol: ProtocolTypeISCSI, want: ProtocolCategoryBlock},
		{name: "nfs is file", protocol: ProtocolTypeNFS, want: ProtocolCategoryFile},
		{name: "smb is file", protocol: ProtocolTypeSMB, want: ProtocolCategoryFile},
		{name: "unknown protocol is unknown", protocol: ProtocolType("mystery"), want: ProtocolCategoryUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ProtocolCategoryOf(tt.protocol)
			if got != tt.want {
				t.Fatalf("ProtocolCategoryOf(%q) = %q, want %q", tt.protocol, got, tt.want)
			}
		})
	}
}

func TestCompatible(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		backend              BackendType
		protocol             ProtocolType
		wantBackendCategory  BackendCategory
		wantProtocolCategory ProtocolCategory
		wantOK               bool
		wantMessage          string
	}{
		{
			name:                 "block backend with block protocol",
			backend:              BackendTypeZFSZvol,
			protocol:             ProtocolTypeNVMeOFTCP,
			wantBackendCategory:  BackendCategoryBlock,
			wantProtocolCategory: ProtocolCategoryBlock,
			wantOK:               true,
			wantMessage:          `Backend type "zfs-zvol" and protocol type "nvmeof-tcp" are compatible`,
		},
		{
			name:                 "filesystem backend with smb",
			backend:              BackendTypeDir,
			protocol:             ProtocolTypeSMB,
			wantBackendCategory:  BackendCategoryFilesystem,
			wantProtocolCategory: ProtocolCategoryFile,
			wantOK:               true,
			wantMessage:          `Backend type "dir" and protocol type "smb" are compatible`,
		},
		{
			name:                 "block backend with nfs is incompatible",
			backend:              BackendTypeLVMLV,
			protocol:             ProtocolTypeNFS,
			wantBackendCategory:  BackendCategoryBlock,
			wantProtocolCategory: ProtocolCategoryFile,
			wantOK:               false,
			wantMessage:          `backend type "lvm-lv" is incompatible with protocol type "nfs": raw block backends cannot be exported via NFS; use zfs-dataset or dir for file protocols (nfs, smb), or switch to a block protocol (nvmeof-tcp, iscsi)`,
		},
		{
			name:                 "filesystem backend with iscsi is incompatible",
			backend:              BackendTypeZFSDataset,
			protocol:             ProtocolTypeISCSI,
			wantBackendCategory:  BackendCategoryFilesystem,
			wantProtocolCategory: ProtocolCategoryBlock,
			wantOK:               false,
			wantMessage:          `backend type "zfs-dataset" is incompatible with protocol type "iscsi": filesystem backends cannot be exposed as block devices; use zfs-zvol or lvm-lv for block protocols, or switch to NFS or SMB`,
		},
		{
			name:                 "unknown values are not compatible",
			backend:              BackendType("mystery"),
			protocol:             ProtocolType("mystery"),
			wantBackendCategory:  BackendCategoryUnknown,
			wantProtocolCategory: ProtocolCategoryUnknown,
			wantOK:               false,
			wantMessage:          `backend type "mystery" is incompatible with protocol type "mystery": compatibility categories could not be evaluated`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Compatible(tt.backend, tt.protocol)
			if got.BackendCategory != tt.wantBackendCategory {
				t.Fatalf("Compatible(%q, %q).BackendCategory = %q, want %q",
					tt.backend, tt.protocol, got.BackendCategory, tt.wantBackendCategory)
			}
			if got.ProtocolCategory != tt.wantProtocolCategory {
				t.Fatalf("Compatible(%q, %q).ProtocolCategory = %q, want %q",
					tt.backend, tt.protocol, got.ProtocolCategory, tt.wantProtocolCategory)
			}
			if got.OK != tt.wantOK {
				t.Fatalf("Compatible(%q, %q).OK = %t, want %t",
					tt.backend, tt.protocol, got.OK, tt.wantOK)
			}
			if got.Message != tt.wantMessage {
				t.Fatalf("Compatible(%q, %q).Message = %q, want %q",
					tt.backend, tt.protocol, got.Message, tt.wantMessage)
			}
		})
	}
}
