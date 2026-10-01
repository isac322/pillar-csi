//go:build integration

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

import (
	"context"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	pillarcsiv1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

// E23 — PillarProtocol CRD lifecycle: webhook validation tests.
//
// The Ginkgo specs call PillarProtocolCustomValidator directly and create
// objects through the envtest API server to show the CRD schema (CEL union
// rule, numeric bounds) and the webhook agree.  The table tests below run
// without envtest (go test -tags integration -run 'TestPillarProtocol').

func nvmeProtocolSpec(cfg pillarcsiv1alpha1.NVMeOFTCPConfig) pillarcsiv1alpha1.PillarProtocolSpec {
	return pillarcsiv1alpha1.PillarProtocolSpec{
		Protocol: pillarcsiv1alpha1.ProtocolSpec{NVMeOFTCP: &cfg},
	}
}

var _ = Describe("PillarProtocol Webhook", func() {
	var (
		obj       *pillarcsiv1alpha1.PillarProtocol
		oldObj    *pillarcsiv1alpha1.PillarProtocol
		validator PillarProtocolCustomValidator
	)

	BeforeEach(func() {
		obj = &pillarcsiv1alpha1.PillarProtocol{}
		oldObj = &pillarcsiv1alpha1.PillarProtocol{}
		validator = PillarProtocolCustomValidator{}
	})

	// ── E23.1: Valid spec creation ────────────────────────────────────────────

	Context("When creating PillarProtocol under Validating Webhook", func() {
		// E23.1.1 — TestPillarProtocolWebhook_ValidCreate_NVMeOFTCP
		It("Should allow creation with spec.protocol.nvmeofTcp", func() {
			obj.Name = "pp-nvmeof-tcp"
			obj.Spec = nvmeProtocolSpec(pillarcsiv1alpha1.NVMeOFTCPConfig{
				Port:              4420,
				MaxQueueSize:      ptr.To[int32](64),
				InCapsuleDataSize: ptr.To[int32](16384),
				CtrlLossTmo:       ptr.To[int32](1200),
				ReconnectDelay:    ptr.To[int32](5),
			})

			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeNil())
		})

		// E23.1.2 — TestPillarProtocolWebhook_InvalidCreate_EmptyUnion
		It("Should deny creation when spec.protocol sets no member", func() {
			obj.Name = "pp-empty"

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.protocol"))
			Expect(err.Error()).To(ContainSubstring("nvmeofTcp"))
		})

		// E23.1.3 — TestPillarProtocolWebhook_InvalidCreate_MaxQueueSizeOutOfRange
		It("Should deny creation when nvmeofTcp.maxQueueSize is outside 16-1024", func() {
			obj.Name = "pp-bad-queue"
			obj.Spec = nvmeProtocolSpec(pillarcsiv1alpha1.NVMeOFTCPConfig{
				Port: 4420, MaxQueueSize: ptr.To[int32](8),
			})

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.protocol.nvmeofTcp.maxQueueSize"))
		})
	})

	// ── E23.2: CRD schema and webhook agree ──────────────────────────────────

	Context("When creating PillarProtocol through the API server", func() {
		It("Should reject an empty protocol union (CEL exactly-one rule)", func() {
			pp := &pillarcsiv1alpha1.PillarProtocol{ObjectMeta: metav1.ObjectMeta{Name: "pp-cel-empty"}}
			Expect(k8sClient.Create(ctx, pp)).NotTo(Succeed())
		})

		It("Should reject maxQueueSize above 1024 (schema maximum)", func() {
			pp := &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: "pp-cel-queue"},
				Spec: nvmeProtocolSpec(pillarcsiv1alpha1.NVMeOFTCPConfig{
					Port: 4420, MaxQueueSize: ptr.To[int32](2048),
				}),
			}
			Expect(k8sClient.Create(ctx, pp)).NotTo(Succeed())
		})
	})

	// ── E23.3: Immutable field update rejection ───────────────────────────────

	Context("When updating PillarProtocol under Validating Webhook", func() {
		// E23.3.1 — TestPillarProtocolWebhook_ImmutableUpdate_MemberRemoved
		It("Should deny update that removes the nvmeofTcp member", func() {
			oldObj.Name = "pp-immutable-nvme"
			oldObj.Spec = nvmeProtocolSpec(pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420})
			obj.Name = "pp-immutable-nvme"

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("immutable"))
			Expect(err.Error()).To(ContainSubstring(`"nvmeofTcp"`))
		})

		// E23.3.3 — TestPillarProtocolWebhook_MutableUpdate_PortChange
		It("Should allow update when only spec.protocol.nvmeofTcp.port changes", func() {
			oldObj.Name = "pp-mutable-port"
			oldObj.Spec = nvmeProtocolSpec(pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420})
			obj.Name = "pp-mutable-port"
			obj.Spec = nvmeProtocolSpec(pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4421})

			warnings, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeNil())
		})

		// E23.3.4 — TestPillarProtocolWebhook_InvalidUpdate_OutOfRange
		It("Should deny update that sets inCapsuleDataSize below 1024", func() {
			oldObj.Spec = nvmeProtocolSpec(pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420})
			obj.Spec = nvmeProtocolSpec(pillarcsiv1alpha1.NVMeOFTCPConfig{
				Port: 4420, InCapsuleDataSize: ptr.To[int32](512),
			})

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.protocol.nvmeofTcp.inCapsuleDataSize"))
		})
	})
})

func TestPillarProtocol_ValidateCreate_NVMeOFTCPDomains(t *testing.T) {
	tests := []struct {
		name     string
		cfg      pillarcsiv1alpha1.NVMeOFTCPConfig
		wantPath string // empty = admitted
	}{
		{name: "defaults", cfg: pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420}},
		{name: "port min", cfg: pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 1}},
		{name: "port max", cfg: pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 65535}},
		{name: "port zero", cfg: pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 0},
			wantPath: "spec.protocol.nvmeofTcp.port"},
		{name: "port too high", cfg: pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 65536},
			wantPath: "spec.protocol.nvmeofTcp.port"},
		{name: "maxQueueSize bounds", cfg: pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420, MaxQueueSize: ptr.To[int32](16)}},
		{name: "maxQueueSize upper bound", cfg: pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420,
			MaxQueueSize: ptr.To[int32](1024)}},
		{name: "maxQueueSize too low", cfg: pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420,
			MaxQueueSize: ptr.To[int32](15)}, wantPath: "spec.protocol.nvmeofTcp.maxQueueSize"},
		{name: "maxQueueSize too high", cfg: pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420,
			MaxQueueSize: ptr.To[int32](1025)}, wantPath: "spec.protocol.nvmeofTcp.maxQueueSize"},
		{name: "inCapsuleDataSize min", cfg: pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420,
			InCapsuleDataSize: ptr.To[int32](1024)}},
		{name: "inCapsuleDataSize too low", cfg: pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420,
			InCapsuleDataSize: ptr.To[int32](1023)}, wantPath: "spec.protocol.nvmeofTcp.inCapsuleDataSize"},
		{name: "ctrlLossTmo zero", cfg: pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420, CtrlLossTmo: ptr.To[int32](0)}},
		{name: "ctrlLossTmo negative", cfg: pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420,
			CtrlLossTmo: ptr.To[int32](-1)}, wantPath: "spec.protocol.nvmeofTcp.ctrlLossTmo"},
		{name: "reconnectDelay negative", cfg: pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420,
			ReconnectDelay: ptr.To[int32](-1)}, wantPath: "spec.protocol.nvmeofTcp.reconnectDelay"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pp := &pillarcsiv1alpha1.PillarProtocol{Spec: nvmeProtocolSpec(tt.cfg)}
			_, err := (&PillarProtocolCustomValidator{}).ValidateCreate(context.Background(), pp)
			if tt.wantPath == "" {
				if err != nil {
					t.Fatalf("ValidateCreate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateCreate() error = nil, want error on %s", tt.wantPath)
			}
			if !strings.Contains(err.Error(), tt.wantPath+":") {
				t.Fatalf("ValidateCreate() error = %v, want path %s", err, tt.wantPath)
			}
		})
	}
}

func TestPillarProtocol_ValidateCreate_ISCSIDomains(t *testing.T) {
	tests := []struct {
		name     string
		spec     pillarcsiv1alpha1.ProtocolSpec
		wantPath string // empty = admitted
	}{
		{name: "defaults", spec: pillarcsiv1alpha1.ProtocolSpec{
			ISCSI: &pillarcsiv1alpha1.ISCSIConfig{Port: 3260}}},
		{name: "all timeouts at their minimum", spec: pillarcsiv1alpha1.ProtocolSpec{
			ISCSI: &pillarcsiv1alpha1.ISCSIConfig{Port: 3260, LoginTimeout: ptr.To[int32](1),
				ReplacementTimeout: ptr.To[int32](0), NoopOutInterval: ptr.To[int32](0),
				NoopOutTimeout: ptr.To[int32](0)}}},
		{name: "port zero", spec: pillarcsiv1alpha1.ProtocolSpec{
			ISCSI: &pillarcsiv1alpha1.ISCSIConfig{Port: 0}}, wantPath: "spec.protocol.iscsi.port"},
		{name: "loginTimeout zero", spec: pillarcsiv1alpha1.ProtocolSpec{
			ISCSI: &pillarcsiv1alpha1.ISCSIConfig{Port: 3260, LoginTimeout: ptr.To[int32](0)}},
			wantPath: "spec.protocol.iscsi.loginTimeout"},
		{name: "replacementTimeout negative", spec: pillarcsiv1alpha1.ProtocolSpec{
			ISCSI: &pillarcsiv1alpha1.ISCSIConfig{Port: 3260, ReplacementTimeout: ptr.To[int32](-1)}},
			wantPath: "spec.protocol.iscsi.replacementTimeout"},
		{name: "noopOutTimeout negative", spec: pillarcsiv1alpha1.ProtocolSpec{
			ISCSI: &pillarcsiv1alpha1.ISCSIConfig{Port: 3260, NoopOutTimeout: ptr.To[int32](-1)}},
			wantPath: "spec.protocol.iscsi.noopOutTimeout"},
		{name: "both members set", spec: pillarcsiv1alpha1.ProtocolSpec{
			NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420},
			ISCSI:     &pillarcsiv1alpha1.ISCSIConfig{Port: 3260}}, wantPath: "spec.protocol"},
		{name: "CHAP with secretRef and acl", spec: pillarcsiv1alpha1.ProtocolSpec{
			ISCSI: &pillarcsiv1alpha1.ISCSIConfig{Port: 3260, ACL: true, Auth: &pillarcsiv1alpha1.ISCSIAuth{
				Method:    pillarcsiv1alpha1.ISCSIAuthMethodCHAP,
				SecretRef: &pillarcsiv1alpha1.ISCSIAuthSecretReference{Name: "chap"}}}}},
		{name: "None without secretRef or acl", spec: pillarcsiv1alpha1.ProtocolSpec{
			ISCSI: &pillarcsiv1alpha1.ISCSIConfig{Port: 3260, Auth: &pillarcsiv1alpha1.ISCSIAuth{
				Method: pillarcsiv1alpha1.ISCSIAuthMethodNone}}}},
		{name: "MutualCHAP without secretRef", spec: pillarcsiv1alpha1.ProtocolSpec{
			ISCSI: &pillarcsiv1alpha1.ISCSIConfig{Port: 3260, ACL: true, Auth: &pillarcsiv1alpha1.ISCSIAuth{
				Method: pillarcsiv1alpha1.ISCSIAuthMethodMutualCHAP}}},
			wantPath: "spec.protocol.iscsi.auth.secretRef"},
		{name: "CHAP without acl", spec: pillarcsiv1alpha1.ProtocolSpec{
			ISCSI: &pillarcsiv1alpha1.ISCSIConfig{Port: 3260, Auth: &pillarcsiv1alpha1.ISCSIAuth{
				Method:    pillarcsiv1alpha1.ISCSIAuthMethodCHAP,
				SecretRef: &pillarcsiv1alpha1.ISCSIAuthSecretReference{Name: "chap"}}}},
			wantPath: "spec.protocol.iscsi.acl"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pp := &pillarcsiv1alpha1.PillarProtocol{Spec: pillarcsiv1alpha1.PillarProtocolSpec{Protocol: tt.spec}}
			_, err := (&PillarProtocolCustomValidator{}).ValidateCreate(context.Background(), pp)
			if tt.wantPath == "" {
				if err != nil {
					t.Fatalf("ValidateCreate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantPath+":") {
				t.Fatalf("ValidateCreate() error = %v, want error on %s", err, tt.wantPath)
			}
		})
	}
}

func TestPillarProtocol_ValidateUpdate_MemberImmutable(t *testing.T) {
	withMember := &pillarcsiv1alpha1.PillarProtocol{Spec: nvmeProtocolSpec(pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420})}
	withISCSI := &pillarcsiv1alpha1.PillarProtocol{Spec: pillarcsiv1alpha1.PillarProtocolSpec{
		Protocol: pillarcsiv1alpha1.ProtocolSpec{ISCSI: &pillarcsiv1alpha1.ISCSIConfig{Port: 3260}},
	}}
	empty := &pillarcsiv1alpha1.PillarProtocol{}

	for name, pair := range map[string][2]*pillarcsiv1alpha1.PillarProtocol{
		"member removed":     {withMember, empty},
		"member added":       {empty, withMember},
		"nvmeofTcp to iscsi": {withMember, withISCSI},
		"iscsi to nvmeofTcp": {withISCSI, withMember},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (&PillarProtocolCustomValidator{}).ValidateUpdate(context.Background(), pair[0], pair[1])
			if err == nil || !strings.Contains(err.Error(), "spec.protocol: Forbidden") {
				t.Fatalf("ValidateUpdate() error = %v, want forbidden spec.protocol", err)
			}
		})
	}
}
