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
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// nqnGeneratePrefix is the NQN prefix used for GenerateNQN.
const nqnGeneratePrefix = "nqn.2026-01.com.bhyoo.pillar-csi:"

// nqnMaxLength is the maximum NQN length per NVMe specification (223 chars).
const nqnMaxLength = 223

// GenerateNQN generates an NVMe Qualified Name from target and volume names.
// Format: nqn.2026-01.com.bhyoo.pillar-csi:<target>:<volume>.
func GenerateNQN(target, volume string) string {
	nqn := nqnGeneratePrefix + target + ":" + volume
	if len(nqn) > nqnMaxLength {
		// Truncate to max length - this is a best-effort approach
		nqn = nqn[:nqnMaxLength]
	}
	return nqn
}

// IsValidNQN returns true if nqn is a syntactically valid NVMe Qualified Name.
// A valid NQN must start with "nqn." prefix.
func IsValidNQN(nqn string) bool {
	return strings.HasPrefix(nqn, "nqn.")
}

// IsValidIQN returns true if iqn is a syntactically valid iSCSI Qualified Name.
// A valid IQN must start with "iqn." prefix.
func IsValidIQN(iqn string) bool {
	return strings.HasPrefix(iqn, "iqn.")
}

// ResolveAddress picks an IP address from a slice of NodeAddresses based on
// address type and optional CIDR filter. Returns the first matching address,
// or error if no match is found.
func ResolveAddress(
	addresses []corev1.NodeAddress,
	addressType corev1.NodeAddressType,
	cidrSelector string,
) (string, error) {
	var cidr *net.IPNet
	if cidrSelector != "" {
		_, parsed, err := net.ParseCIDR(cidrSelector)
		if err != nil {
			return "", fmt.Errorf("invalid CIDR %q: %w", cidrSelector, err)
		}
		cidr = parsed
	}

	for _, addr := range addresses {
		if addr.Type != addressType {
			continue
		}
		ip := net.ParseIP(addr.Address)
		if ip == nil {
			continue
		}
		if cidr != nil && !cidr.Contains(ip) {
			continue
		}
		return addr.Address, nil
	}

	if cidrSelector != "" {
		return "", fmt.Errorf("no %q address within CIDR %q", addressType, cidrSelector)
	}
	return "", fmt.Errorf("no %q address found", addressType)
}
