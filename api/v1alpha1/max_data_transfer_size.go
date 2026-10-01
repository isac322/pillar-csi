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

// NVMeOFTCPConfig.maxDataTransferSize domain.  The CRD enum markers list the
// same values; IsValidMaxDataTransferSize is the check every other layer
// (webhook, override documents) applies.
const (
	// DefaultMaxDataTransferSize is the value an unset maxDataTransferSize
	// resolves to: 4 MiB per command needs a 32 KiB contiguous scatterlist
	// allocation on the target instead of 256 KiB for the 32 MiB a recent
	// host sends to a target without a limit.
	DefaultMaxDataTransferSize int32 = 4 << 20

	// MinMaxDataTransferSize is the smallest limit: two 4 KiB pages.  The
	// nvmet port's param_mdts holds log2(size / 4 KiB), and its value 0
	// means no limit, so one page cannot be expressed.
	MinMaxDataTransferSize int32 = 8192

	// MaxMaxDataTransferSize is the largest power of two an int32 holds.
	MaxMaxDataTransferSize int32 = 1 << 30
)

// IsValidMaxDataTransferSize reports whether v is an accepted
// maxDataTransferSize: 0 (no limit) or a power of two from
// MinMaxDataTransferSize to MaxMaxDataTransferSize.
func IsValidMaxDataTransferSize(v int64) bool {
	if v == 0 {
		return true
	}
	return v >= int64(MinMaxDataTransferSize) && v <= int64(MaxMaxDataTransferSize) && v&(v-1) == 0
}
