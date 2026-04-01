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

package v1alpha1

import "fmt"

// BackendCategory classifies a backend by the kind of volume it produces.
type BackendCategory string

const (
	// BackendCategoryUnknown represents an unrecognized backend type.
	BackendCategoryUnknown BackendCategory = "unknown"

	// BackendCategoryBlock represents raw block-device backends.
	BackendCategoryBlock BackendCategory = "block"

	// BackendCategoryFilesystem represents filesystem/directory backends.
	BackendCategoryFilesystem BackendCategory = "filesystem"
)

// CategoryOf returns the compatibility category for a backend type.
func CategoryOf(bt BackendType) BackendCategory {
	switch bt {
	case BackendTypeZFSZvol, BackendTypeLVMLV:
		return BackendCategoryBlock
	case BackendTypeZFSDataset, BackendTypeDir:
		return BackendCategoryFilesystem
	default:
		return BackendCategoryUnknown
	}
}

// ProtocolCategory classifies a protocol by the kind of volume it exports.
type ProtocolCategory string

const (
	// ProtocolCategoryUnknown represents an unrecognized protocol type.
	ProtocolCategoryUnknown ProtocolCategory = "unknown"

	// ProtocolCategoryBlock represents block-storage protocols.
	ProtocolCategoryBlock ProtocolCategory = "block"

	// ProtocolCategoryFile represents file-storage protocols.
	ProtocolCategoryFile ProtocolCategory = "file"
)

// ProtocolCategoryOf returns the compatibility category for a protocol type.
func ProtocolCategoryOf(pt ProtocolType) ProtocolCategory {
	switch pt {
	case ProtocolTypeNVMeOFTCP, ProtocolTypeISCSI:
		return ProtocolCategoryBlock
	case ProtocolTypeNFS, ProtocolTypeSMB:
		return ProtocolCategoryFile
	default:
		return ProtocolCategoryUnknown
	}
}

// Compatibility describes the compatibility verdict for a backend/protocol pair.
type Compatibility struct {
	BackendType      BackendType
	BackendCategory  BackendCategory
	ProtocolType     ProtocolType
	ProtocolCategory ProtocolCategory
	OK               bool
	Message          string
}

// Compatible evaluates whether a backend type can be served over a protocol type.
func Compatible(bt BackendType, pt ProtocolType) Compatibility {
	result := Compatibility{
		BackendType:      bt,
		BackendCategory:  CategoryOf(bt),
		ProtocolType:     pt,
		ProtocolCategory: ProtocolCategoryOf(pt),
	}

	result.OK = result.BackendCategory != BackendCategoryUnknown &&
		result.ProtocolCategory != ProtocolCategoryUnknown &&
		((result.BackendCategory == BackendCategoryBlock && result.ProtocolCategory == ProtocolCategoryBlock) ||
			(result.BackendCategory == BackendCategoryFilesystem && result.ProtocolCategory == ProtocolCategoryFile))

	switch {
	case result.OK:
		result.Message = fmt.Sprintf(
			"Backend type %q and protocol type %q are compatible",
			bt, pt,
		)
	case result.BackendCategory == BackendCategoryBlock && result.ProtocolCategory == ProtocolCategoryFile:
		result.Message = fmt.Sprintf(
			"backend type %q is incompatible with protocol type %q: raw block backends cannot be exported via %s; "+
				"use zfs-dataset or dir for file protocols (nfs, smb), or switch to a block protocol (nvmeof-tcp, iscsi)",
			bt, pt, protocolDisplayName(pt),
		)
	case result.BackendCategory == BackendCategoryFilesystem && result.ProtocolCategory == ProtocolCategoryBlock:
		result.Message = fmt.Sprintf(
			"backend type %q is incompatible with protocol type %q: filesystem backends cannot be exposed as block devices; "+
				"use zfs-zvol or lvm-lv for block protocols, or switch to NFS or SMB",
			bt, pt,
		)
	default:
		result.Message = fmt.Sprintf(
			"backend type %q is incompatible with protocol type %q: compatibility categories could not be evaluated",
			bt, pt,
		)
	}

	return result
}

func protocolDisplayName(pt ProtocolType) string {
	switch pt {
	case ProtocolTypeNFS:
		return "NFS"
	case ProtocolTypeSMB:
		return "SMB"
	default:
		return string(pt)
	}
}
