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

// Package unit_test contains unit tests for pure-logic functions in
// pillar-csi. No external dependencies (K8s API, gRPC, kernel modules)
// are required.
//
// Run with:
//
//	go test ./test/unit/ -v
package unit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	corev1 "k8s.io/api/core/v1"

	pillarcsi "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// NEW-U1: addressSelector CIDR 매칭
// ─────────────────────────────────────────────────────────────────────────────

// TestResolveAddress_CIDRFilter_MatchesSubnet verifies that ResolveAddress
// returns the first address that matches the given CIDR filter.
func TestResolveAddress_CIDRFilter_MatchesSubnet(t *testing.T) {
	t.Parallel()

	addresses := []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
		{Type: corev1.NodeInternalIP, Address: "192.168.219.6"},
	}

	got, err := pillarcsi.ResolveAddress(addresses, corev1.NodeInternalIP, "192.168.219.0/24")
	if err != nil {
		t.Fatalf("ResolveAddress: unexpected error: %v", err)
	}
	const want = "192.168.219.6"
	if got != want {
		t.Errorf("ResolveAddress = %q, want %q", got, want)
	}
}

// TestResolveAddress_CIDRFilter_NoMatch verifies that ResolveAddress returns
// an error when no address matches the CIDR filter.
func TestResolveAddress_CIDRFilter_NoMatch(t *testing.T) {
	t.Parallel()

	addresses := []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
	}

	_, err := pillarcsi.ResolveAddress(addresses, corev1.NodeInternalIP, "192.168.0.0/16")
	if err == nil {
		t.Fatal("ResolveAddress: expected error for no-match CIDR, got nil")
	}
}

// TestResolveAddress_NoCIDR_FirstMatch verifies that when no CIDR is given,
// ResolveAddress returns the first address matching addressType.
func TestResolveAddress_NoCIDR_FirstMatch(t *testing.T) {
	t.Parallel()

	addresses := []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
		{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
	}

	got, err := pillarcsi.ResolveAddress(addresses, corev1.NodeInternalIP, "")
	if err != nil {
		t.Fatalf("ResolveAddress: unexpected error: %v", err)
	}
	const want = "10.0.0.5"
	if got != want {
		t.Errorf("ResolveAddress = %q, want %q (first InternalIP)", got, want)
	}
}

// TestResolveAddress_InvalidCIDR verifies that a malformed CIDR returns an
// error without panicking.
func TestResolveAddress_InvalidCIDR(t *testing.T) {
	t.Parallel()

	addresses := []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
	}

	_, err := pillarcsi.ResolveAddress(addresses, corev1.NodeInternalIP, "not-a-cidr")
	if err == nil {
		t.Fatal("ResolveAddress: expected error for invalid CIDR, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NEW-U2: CSI Topology Capability 미선언
// ─────────────────────────────────────────────────────────────────────────────

// TestPluginCapabilities_NoTopology verifies that GetPluginCapabilities does
// not advertise VOLUME_ACCESSIBILITY_CONSTRAINTS (topology), which is not
// supported in Phase 1 of pillar-csi.
func TestPluginCapabilities_NoTopology(t *testing.T) {
	t.Parallel()

	srv := pillarcsi.NewIdentityServer("pillar-csi.bhyoo.com", "0.1.0")
	resp, err := srv.GetPluginCapabilities(context.Background(), &csipb.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities: unexpected error: %v", err)
	}

	for _, cap := range resp.GetCapabilities() {
		if svc, ok := cap.Type.(*csipb.PluginCapability_Service_); ok {
			if svc.Service.Type == csipb.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS {
				t.Errorf("GetPluginCapabilities returned VOLUME_ACCESSIBILITY_CONSTRAINTS; " +
					"this capability is not yet supported in Phase 1")
			}
		}
	}

	// Verify CONTROLLER_SERVICE and VolumeExpansion_ONLINE are present.
	var hasController, hasExpansion bool
	for _, cap := range resp.GetCapabilities() {
		switch v := cap.Type.(type) {
		case *csipb.PluginCapability_Service_:
			if v.Service.Type == csipb.PluginCapability_Service_CONTROLLER_SERVICE {
				hasController = true
			}
		case *csipb.PluginCapability_VolumeExpansion_:
			if v.VolumeExpansion.Type == csipb.PluginCapability_VolumeExpansion_ONLINE {
				hasExpansion = true
			}
		}
	}
	if !hasController {
		t.Error("GetPluginCapabilities: CONTROLLER_SERVICE capability missing")
	}
	if !hasExpansion {
		t.Error("GetPluginCapabilities: VolumeExpansion_ONLINE capability missing")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NEW-U3: 구조화된 로깅 형식 (JSON, slog)
// ─────────────────────────────────────────────────────────────────────────────

// TestStructuredLogging_JSONFormat verifies that slog with JSONHandler outputs
// valid JSON containing the expected message and key-value fields.
func TestStructuredLogging_JSONFormat(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("test", "key", "value")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("slog JSON output is not valid JSON: %v\noutput: %s", err, buf.String())
	}
	if got := m["msg"]; got != "test" {
		t.Errorf("JSON[\"msg\"] = %q, want \"test\"", got)
	}
	if got := m["key"]; got != "value" {
		t.Errorf("JSON[\"key\"] = %q, want \"value\"", got)
	}
}

// TestStructuredLogging_ErrorLevel verifies that slog error logging includes
// the "ERROR" level and the error message in the JSON output.
func TestStructuredLogging_ErrorLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Error("fail", "err", errors.New("boom"))

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("slog JSON output is not valid JSON: %v\noutput: %s", err, buf.String())
	}
	if got := m["level"]; got != "ERROR" {
		t.Errorf("JSON[\"level\"] = %q, want \"ERROR\"", got)
	}
	if got, ok := m["err"].(string); !ok || !strings.Contains(got, "boom") {
		t.Errorf("JSON[\"err\"] = %q, want string containing \"boom\"", m["err"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NEW-U4: NQN 형식 생성
// ─────────────────────────────────────────────────────────────────────────────

// TestGenerateNQN_Format verifies that GenerateNQN produces an NQN in the
// correct format: nqn.2026-01.com.bhyoo.pillar-csi:<target>:<volume>.
func TestGenerateNQN_Format(t *testing.T) {
	t.Parallel()

	got := pillarcsi.GenerateNQN("rock5bp", "pvc-abc123")
	const want = "nqn.2026-01.com.bhyoo.pillar-csi:rock5bp:pvc-abc123"
	if got != want {
		t.Errorf("GenerateNQN = %q, want %q", got, want)
	}
}

// TestGenerateNQN_MaxLength verifies that GenerateNQN never returns a string
// longer than 223 characters (NVMe specification limit).
func TestGenerateNQN_MaxLength(t *testing.T) {
	t.Parallel()

	// Create inputs that would produce an NQN exceeding 223 chars.
	longTarget := strings.Repeat("a", 100)
	longVolume := strings.Repeat("b", 200)

	got := pillarcsi.GenerateNQN(longTarget, longVolume)
	const maxLen = 223
	if len(got) > maxLen {
		t.Errorf("GenerateNQN length = %d, want ≤ %d", len(got), maxLen)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NEW-U5: NQN/IQN 형식 검증
// ─────────────────────────────────────────────────────────────────────────────

// TestIsValidNQN_ValidFormat verifies that IsValidNQN accepts a well-formed NQN.
func TestIsValidNQN_ValidFormat(t *testing.T) {
	t.Parallel()

	const nqn = "nqn.2014-08.org.nvmexpress:uuid:1234-5678"
	if !pillarcsi.IsValidNQN(nqn) {
		t.Errorf("IsValidNQN(%q) = false, want true", nqn)
	}
}

// TestIsValidNQN_InvalidFormat verifies that IsValidNQN rejects a string that
// does not start with the "nqn." prefix.
func TestIsValidNQN_InvalidFormat(t *testing.T) {
	t.Parallel()

	const nqn = "not-a-nqn"
	if pillarcsi.IsValidNQN(nqn) {
		t.Errorf("IsValidNQN(%q) = true, want false", nqn)
	}
}

// TestIsValidIQN_ValidFormat verifies that IsValidIQN accepts a well-formed IQN.
func TestIsValidIQN_ValidFormat(t *testing.T) {
	t.Parallel()

	const iqn = "iqn.1993-08.org.debian:01:abcdef"
	if !pillarcsi.IsValidIQN(iqn) {
		t.Errorf("IsValidIQN(%q) = false, want true", iqn)
	}
}
