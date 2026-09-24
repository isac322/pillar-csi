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

package controller

// The controller-gen ClusterRole (config/rbac/role.yaml, generated from
// +kubebuilder:rbac markers) must authorize every PillarVolumeState operation
// the CSI controller performs, as evaluated by a real API server against the
// controller-gen CRD this suite installs from config/crd/bases. The chart
// ClusterRole is required to cover role.yaml by charts/pillar-csi/test_render.sh.

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

var _ = Describe("PillarVolumeState RBAC contract", func() {
	const subject = "system:serviceaccount:rbac-contract:pillar-csi-controller"

	It("lets the generated manager role perform every PillarVolumeState operation of the CSI controller", func() {
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
		Expect(err).NotTo(HaveOccurred())
		role := &rbacv1.ClusterRole{}
		Expect(yaml.UnmarshalStrict(raw, role)).To(Succeed())
		role.Name = "pvs-rbac-contract"
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, role))).To(Succeed()) })

		binding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "pvs-rbac-contract"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: subject}},
		}
		Expect(k8sClient.Create(ctx, binding)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, binding))).To(Succeed()) })

		impersonated := rest.CopyConfig(cfg)
		impersonated.Impersonate = rest.ImpersonationConfig{UserName: subject}
		c, err := client.New(impersonated, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())

		// The RBAC authorizer observes new bindings asynchronously.
		Eventually(func() error {
			return c.List(ctx, &pillarcsiv1alpha1.PillarVolumeStateList{})
		}).WithTimeout(10 * time.Second).Should(Succeed())

		pvs := &pillarcsiv1alpha1.PillarVolumeState{
			ObjectMeta: metav1.ObjectMeta{Name: "pvs-rbac-contract"},
			Spec: pillarcsiv1alpha1.PillarVolumeStateSpec{
				VolumeID:      "agent-a/nvmeof-tcp/lvm-lv/vg/pvc-rbac",
				AgentRef:      "agent-a",
				AgentVolumeID: "vg/pvc-rbac",
				BackendType:   "lvm-lv",
				ProtocolType:  "nvmeof-tcp",
			},
		}
		Expect(c.Create(ctx, pvs)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pvs))).To(Succeed()) })

		got := &pillarcsiv1alpha1.PillarVolumeState{}
		Expect(c.Get(ctx, types.NamespacedName{Name: pvs.Name}, got)).To(Succeed())
		got.Status.Phase = pillarcsiv1alpha1.PillarVolumeStatePhaseCreatePartial
		Expect(c.Status().Update(ctx, got)).To(Succeed())
		Expect(c.Delete(ctx, got)).To(Succeed())
	})
})
