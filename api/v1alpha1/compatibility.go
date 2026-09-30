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
	// BackendCategoryUnknown represents an unrecognized backend.
	BackendCategoryUnknown BackendCategory = "unknown"

	// BackendCategoryBlock represents raw block-device backends.
	BackendCategoryBlock BackendCategory = "block"
)

// CategoryOf returns the compatibility category for a backend kind.
func CategoryOf(b BackendID) BackendCategory {
	switch b {
	case BackendIDZFSZvol, BackendIDLVMLV:
		return BackendCategoryBlock
	default:
		return BackendCategoryUnknown
	}
}

// ProtocolCategory classifies a protocol by the kind of volume it exports.
type ProtocolCategory string

const (
	// ProtocolCategoryUnknown represents an unrecognized protocol.
	ProtocolCategoryUnknown ProtocolCategory = "unknown"

	// ProtocolCategoryBlock represents block-storage protocols.
	ProtocolCategoryBlock ProtocolCategory = "block"
)

// ProtocolCategoryOf returns the compatibility category for a protocol kind.
func ProtocolCategoryOf(p ProtocolID) ProtocolCategory {
	switch p {
	case ProtocolIDNVMeOFTCP, ProtocolIDISCSI:
		return ProtocolCategoryBlock
	default:
		return ProtocolCategoryUnknown
	}
}

// Compatibility describes the compatibility verdict for a backend/protocol pair.
type Compatibility struct {
	BackendID        BackendID
	BackendCategory  BackendCategory
	ProtocolID       ProtocolID
	ProtocolCategory ProtocolCategory
	OK               bool
	Message          string
}

// Compatible evaluates whether the backend selected by spec can be served
// over the protocol selected by pspec: both must be a served variant and
// export the same kind of volume.
func Compatible(spec BackendSpec, pspec ProtocolSpec) Compatibility {
	bt := spec.Kind()
	pt := pspec.Kind()
	result := Compatibility{
		BackendID:        bt,
		BackendCategory:  CategoryOf(bt),
		ProtocolID:       pt,
		ProtocolCategory: ProtocolCategoryOf(pt),
	}

	switch {
	case result.BackendCategory == BackendCategoryUnknown || result.ProtocolCategory == ProtocolCategoryUnknown:
		result.Message = fmt.Sprintf(
			"backend %q is incompatible with protocol %q: backend or protocol is not supported", bt, pt)
	case string(result.BackendCategory) != string(result.ProtocolCategory):
		result.Message = fmt.Sprintf(
			"backend %q (%s volumes) is incompatible with protocol %q (%s volumes)",
			bt, result.BackendCategory, pt, result.ProtocolCategory)
	default:
		result.OK = true
		result.Message = fmt.Sprintf("backend %q and protocol %q are compatible", bt, pt)
	}
	return result
}
