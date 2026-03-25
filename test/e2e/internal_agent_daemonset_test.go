//go:build e2e
// +build e2e

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

// internal_agent_daemonset_test.go — DaemonSet readiness validation for the
// pillar-csi "internal agent" mode.
//
// This file contains the concrete specs for verifying that:
//  1. The pillar-csi Helm release deploys successfully (agent + node DaemonSets exist).
//  2. All DaemonSet pods reach the Running phase on the expected Kind nodes.
//  3. Every agent and node container passes its readiness check
//     (ContainerStatuses.Ready == true, Pod.Status.Phase == Running).
//
// These specs live alongside internal_agent_test.go and share the same
// "internal-agent" Ginkgo label.  The suite lifecycle (Helm install / uninstall)
// is managed by the InternalAgentSuite in internal_agent_test.go; this file
// contains only observational specs that query the Kubernetes API.
//
// # Running these specs in isolation
//
//	go test -tags=e2e -v -count=1 ./test/e2e/ \
//	    -run TestInternalAgent \
//	    -- --ginkgo.label-filter=internal-agent --ginkgo.v
//
// # Prerequisites
//
// The Kind cluster must already be bootstrapped and the Helm chart deployed:
//
//	hack/e2e-setup.sh
//	export KUBECONFIG=$(kind get kubeconfig --name pillar-csi-e2e)
//
// # Architecture notes
//
// The specs do NOT modify any cluster state — they only read DaemonSet and Pod
// objects.  This makes them safe to run concurrently with other suites and
// idempotent across multiple invocations against the same cluster.
//
// Pod membership is determined by fetching the DaemonSet's own
// Spec.Selector.MatchLabels, which is the canonical label query used by the
// Kubernetes scheduler.  This avoids hard-coding label key names that might
// change between chart versions.
package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// ─── Constants ───────────────────────────────────────────────────────────────

const (
	// dsNamespace is the Kubernetes namespace where the pillar-csi Helm release
	// installs its resources.  Matches the default value of HELM_NAMESPACE in
	// hack/e2e-setup.sh and internalAgentNamespace in internal_agent_test.go.
	dsNamespace = "pillar-csi-system"

	// dsAgentName is the name of the agent DaemonSet created by the Helm chart
	// when the release name is "pillar-csi".
	// Formula: <release>-agent = pillar-csi-agent.
	dsAgentName = "pillar-csi-agent"

	// dsNodeName is the name of the node DaemonSet created by the Helm chart
	// when the release name is "pillar-csi".
	// Formula: <release>-node = pillar-csi-node.
	dsNodeName = "pillar-csi-node"

	// dsStorageNodeLabel is the node label used by the agent DaemonSet's
	// nodeSelector to limit scheduling to storage-side Kind nodes.
	// Set by hack/kind-config.yaml on the "storage-worker" node.
	dsStorageNodeLabel = "pillar-csi.bhyoo.com/storage-node"

	// dsReadinessTimeout is the maximum duration the specs will wait for
	// DaemonSet pods to become Ready.  This budget is separate from (and additive
	// to) the 5-minute `--wait` timeout passed to `helm upgrade` in BeforeSuite.
	dsReadinessTimeout = 4 * time.Minute

	// dsPollInterval is the time between successive status polls inside
	// Eventually blocks.
	dsPollInterval = 5 * time.Second

	// dsConnectTimeout is the maximum time to wait for the Kubernetes API server
	// to respond during suite setup.
	dsConnectTimeout = 30 * time.Second
)

// ─── Suite-level state ───────────────────────────────────────────────────────

// dsClient is the controller-runtime client used by all specs in this file.
// Populated once in BeforeAll and read by every spec.
var dsClient client.Client

// ─── Ginkgo container ────────────────────────────────────────────────────────

var _ = Describe("InternalAgent DaemonSet Readiness", Ordered, Label("internal-agent"), func() {

	// BeforeAll establishes cluster connectivity.  It intentionally does NOT
	// deploy or uninstall the Helm chart — that lifecycle is owned by the
	// InternalAgentSuite in internal_agent_test.go.
	BeforeAll(func(ctx context.Context) {
		By("connecting to the Kind cluster for DaemonSet readiness validation")
		suite, err := framework.SetupSuite(framework.WithConnectTimeout(dsConnectTimeout))
		Expect(err).NotTo(HaveOccurred(),
			"DaemonSet readiness: cluster connectivity check failed — "+
				"ensure KUBECONFIG is set and 'hack/e2e-setup.sh' has been run")
		dsClient = suite.Client
	})

	// ─── Helm release objects ───────────────────────────────────────────────

	Describe("Helm release objects", func() {
		It("creates the agent DaemonSet in the pillar-csi-system namespace", func(ctx context.Context) {
			ds := &appsv1.DaemonSet{}
			err := dsClient.Get(ctx, client.ObjectKey{
				Name:      dsAgentName,
				Namespace: dsNamespace,
			}, ds)
			Expect(err).NotTo(HaveOccurred(),
				"agent DaemonSet %q must exist in namespace %q — "+
					"verify that 'hack/e2e-setup.sh' completed without error",
				dsAgentName, dsNamespace)
			By(fmt.Sprintf("agent DaemonSet found: desired=%d current=%d ready=%d",
				ds.Status.DesiredNumberScheduled,
				ds.Status.CurrentNumberScheduled,
				ds.Status.NumberReady,
			))
		})

		It("creates the node DaemonSet in the pillar-csi-system namespace", func(ctx context.Context) {
			ds := &appsv1.DaemonSet{}
			err := dsClient.Get(ctx, client.ObjectKey{
				Name:      dsNodeName,
				Namespace: dsNamespace,
			}, ds)
			Expect(err).NotTo(HaveOccurred(),
				"node DaemonSet %q must exist in namespace %q",
				dsNodeName, dsNamespace)
			By(fmt.Sprintf("node DaemonSet found: desired=%d current=%d ready=%d",
				ds.Status.DesiredNumberScheduled,
				ds.Status.CurrentNumberScheduled,
				ds.Status.NumberReady,
			))
		})
	})

	// ─── Agent DaemonSet scheduling ────────────────────────────────────────

	Describe("agent DaemonSet scheduling", func() {
		It("is scheduled on at least one storage node", func(ctx context.Context) {
			// Count nodes labelled as storage nodes.
			nodeList := &corev1.NodeList{}
			Expect(dsClient.List(ctx, nodeList,
				client.MatchingLabels{dsStorageNodeLabel: "true"},
			)).To(Succeed(), "list nodes with label %s=true", dsStorageNodeLabel)

			storageNodeCount := len(nodeList.Items)
			Expect(storageNodeCount).To(BeNumerically(">", 0),
				"expected at least one node labelled %s=true; "+
					"check hack/kind-config.yaml for the storage-worker node label", dsStorageNodeLabel)

			By(fmt.Sprintf("found %d storage node(s) with label %s=true", storageNodeCount, dsStorageNodeLabel))
		})

		It("DesiredNumberScheduled matches the number of storage-node-labelled Kind nodes", func(ctx context.Context) {
			nodeList := &corev1.NodeList{}
			Expect(dsClient.List(ctx, nodeList,
				client.MatchingLabels{dsStorageNodeLabel: "true"},
			)).To(Succeed())
			storageNodeCount := len(nodeList.Items)

			ds := &appsv1.DaemonSet{}
			Expect(dsClient.Get(ctx, client.ObjectKey{
				Name:      dsAgentName,
				Namespace: dsNamespace,
			}, ds)).To(Succeed())

			Expect(int(ds.Status.DesiredNumberScheduled)).To(Equal(storageNodeCount),
				"agent DaemonSet DesiredNumberScheduled (%d) must equal the number of "+
					"nodes labelled %s=true (%d)",
				ds.Status.DesiredNumberScheduled, dsStorageNodeLabel, storageNodeCount)
		})
	})

	// ─── Agent DaemonSet rollout readiness ─────────────────────────────────

	Describe("agent DaemonSet rollout", func() {
		It("reaches full readiness (all pods Running) within the timeout", func(ctx context.Context) {
			Eventually(func(g Gomega) {
				ds := &appsv1.DaemonSet{}
				g.Expect(dsClient.Get(ctx, client.ObjectKey{
					Name:      dsAgentName,
					Namespace: dsNamespace,
				}, ds)).To(Succeed())

				g.Expect(ds.Status.DesiredNumberScheduled).To(BeNumerically(">", 0),
					"agent DaemonSet must be scheduled on at least one node")
				g.Expect(ds.Status.NumberUnavailable).To(BeZero(),
					"agent DaemonSet must have 0 unavailable pods (got %d unavailable, %d/%d ready)",
					ds.Status.NumberUnavailable,
					ds.Status.NumberReady,
					ds.Status.DesiredNumberScheduled,
				)
				g.Expect(ds.Status.NumberReady).To(Equal(ds.Status.DesiredNumberScheduled),
					"agent DaemonSet NumberReady (%d) must equal DesiredNumberScheduled (%d)",
					ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
				g.Expect(ds.Status.UpdatedNumberScheduled).To(Equal(ds.Status.DesiredNumberScheduled),
					"agent DaemonSet UpdatedNumberScheduled (%d) must equal DesiredNumberScheduled (%d)",
					ds.Status.UpdatedNumberScheduled, ds.Status.DesiredNumberScheduled)
			}, dsReadinessTimeout, dsPollInterval).Should(Succeed(),
				"agent DaemonSet %q did not reach full readiness within %s",
				dsAgentName, dsReadinessTimeout)
		})
	})

	// ─── Agent pod container-level checks ──────────────────────────────────

	Describe("agent DaemonSet pods", func() {
		It("all pods are in the Running phase", func(ctx context.Context) {
			pods, err := dsFetchPods(ctx, dsClient, dsAgentName, dsNamespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).NotTo(BeEmpty(),
				"agent DaemonSet must have at least one pod")

			for i := range pods {
				pod := &pods[i]
				By(fmt.Sprintf("checking pod %s on node %s", pod.Name, pod.Spec.NodeName))
				Expect(pod.Status.Phase).To(Equal(corev1.PodRunning),
					"agent pod %q on node %q must be in Running phase (got %s)",
					pod.Name, pod.Spec.NodeName, pod.Status.Phase)
			}
		})

		It("all pods have the Ready condition == True (liveness/readiness probes passing)", func(ctx context.Context) {
			pods, err := dsFetchPods(ctx, dsClient, dsAgentName, dsNamespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).NotTo(BeEmpty(), "agent DaemonSet must have at least one pod")

			for i := range pods {
				pod := &pods[i]
				By(fmt.Sprintf("checking Ready condition of pod %s", pod.Name))

				readyCond := dsPodCondition(*pod, corev1.PodReady)
				Expect(readyCond).NotTo(BeNil(),
					"agent pod %q must have a Ready condition", pod.Name)
				Expect(readyCond.Status).To(Equal(corev1.ConditionTrue),
					"agent pod %q Ready condition must be True (got %s; message: %q)",
					pod.Name, readyCond.Status, readyCond.Message)
			}
		})

		It("all containers are Ready (ContainerStatuses.Ready)", func(ctx context.Context) {
			pods, err := dsFetchPods(ctx, dsClient, dsAgentName, dsNamespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).NotTo(BeEmpty(), "agent DaemonSet must have at least one pod")

			for i := range pods {
				pod := &pods[i]
				By(fmt.Sprintf("checking container statuses in pod %s", pod.Name))
				Expect(pod.Status.ContainerStatuses).NotTo(BeEmpty(),
					"pod %q must have at least one container status", pod.Name)

				for _, cs := range pod.Status.ContainerStatuses {
					Expect(cs.Ready).To(BeTrue(),
						"container %q in agent pod %q must be Ready "+
							"(state: %s; restarts: %d)",
						cs.Name, pod.Name,
						dsContainerStateString(cs.State),
						cs.RestartCount,
					)
					Expect(cs.State.Running).NotTo(BeNil(),
						"container %q in agent pod %q must be in Running state "+
							"(current state: %s)",
						cs.Name, pod.Name, dsContainerStateString(cs.State))
				}
			}
		})

		It("all init containers completed successfully (exit 0)", func(ctx context.Context) {
			pods, err := dsFetchPods(ctx, dsClient, dsAgentName, dsNamespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).NotTo(BeEmpty(), "agent DaemonSet must have at least one pod")

			for i := range pods {
				pod := &pods[i]
				By(fmt.Sprintf("checking init container statuses in pod %s", pod.Name))
				// init containers are optional; skip if there are none.
				for _, cs := range pod.Status.InitContainerStatuses {
					Expect(cs.State.Terminated).NotTo(BeNil(),
						"init container %q in pod %q must have terminated",
						cs.Name, pod.Name)
					Expect(cs.State.Terminated.ExitCode).To(BeZero(),
						"init container %q in pod %q must have exited 0 (got %d; reason: %s)",
						cs.Name, pod.Name,
						cs.State.Terminated.ExitCode,
						cs.State.Terminated.Reason,
					)
				}
			}
		})
	})

	// ─── Node DaemonSet rollout readiness ──────────────────────────────────

	Describe("node DaemonSet rollout", func() {
		It("reaches full readiness (all pods Running) within the timeout", func(ctx context.Context) {
			Eventually(func(g Gomega) {
				ds := &appsv1.DaemonSet{}
				g.Expect(dsClient.Get(ctx, client.ObjectKey{
					Name:      dsNodeName,
					Namespace: dsNamespace,
				}, ds)).To(Succeed())

				g.Expect(ds.Status.DesiredNumberScheduled).To(BeNumerically(">", 0),
					"node DaemonSet must be scheduled on at least one node")
				g.Expect(ds.Status.NumberUnavailable).To(BeZero(),
					"node DaemonSet must have 0 unavailable pods (got %d unavailable, %d/%d ready)",
					ds.Status.NumberUnavailable,
					ds.Status.NumberReady,
					ds.Status.DesiredNumberScheduled,
				)
				g.Expect(ds.Status.NumberReady).To(Equal(ds.Status.DesiredNumberScheduled),
					"node DaemonSet NumberReady (%d) must equal DesiredNumberScheduled (%d)",
					ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
			}, dsReadinessTimeout, dsPollInterval).Should(Succeed(),
				"node DaemonSet %q did not reach full readiness within %s",
				dsNodeName, dsReadinessTimeout)
		})
	})

	// ─── Node pod container-level checks ───────────────────────────────────

	Describe("node DaemonSet pods", func() {
		It("all pods are in the Running phase", func(ctx context.Context) {
			pods, err := dsFetchPods(ctx, dsClient, dsNodeName, dsNamespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).NotTo(BeEmpty(), "node DaemonSet must have at least one pod")

			for i := range pods {
				pod := &pods[i]
				By(fmt.Sprintf("checking pod %s on node %s", pod.Name, pod.Spec.NodeName))
				Expect(pod.Status.Phase).To(Equal(corev1.PodRunning),
					"node pod %q on node %q must be in Running phase (got %s)",
					pod.Name, pod.Spec.NodeName, pod.Status.Phase)
			}
		})

		It("all pods have the Ready condition == True (liveness probe passing)", func(ctx context.Context) {
			pods, err := dsFetchPods(ctx, dsClient, dsNodeName, dsNamespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).NotTo(BeEmpty(), "node DaemonSet must have at least one pod")

			for i := range pods {
				pod := &pods[i]
				By(fmt.Sprintf("checking Ready condition of pod %s", pod.Name))

				readyCond := dsPodCondition(*pod, corev1.PodReady)
				Expect(readyCond).NotTo(BeNil(),
					"node pod %q must have a Ready condition", pod.Name)
				Expect(readyCond.Status).To(Equal(corev1.ConditionTrue),
					"node pod %q Ready condition must be True (got %s; message: %q)",
					pod.Name, readyCond.Status, readyCond.Message)
			}
		})

		It("all containers are Ready (ContainerStatuses.Ready)", func(ctx context.Context) {
			pods, err := dsFetchPods(ctx, dsClient, dsNodeName, dsNamespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).NotTo(BeEmpty(), "node DaemonSet must have at least one pod")

			for i := range pods {
				pod := &pods[i]
				By(fmt.Sprintf("checking container statuses in pod %s", pod.Name))
				Expect(pod.Status.ContainerStatuses).NotTo(BeEmpty(),
					"pod %q must have at least one container status", pod.Name)

				for _, cs := range pod.Status.ContainerStatuses {
					Expect(cs.Ready).To(BeTrue(),
						"container %q in node pod %q must be Ready "+
							"(state: %s; restarts: %d)",
						cs.Name, pod.Name,
						dsContainerStateString(cs.State),
						cs.RestartCount,
					)
				}
			}
		})
	})
})

// ─── Helper functions ────────────────────────────────────────────────────────

// dsFetchPods returns the pods that belong to the named DaemonSet in the given
// namespace.  Pod membership is determined using the DaemonSet's own
// Spec.Selector.MatchLabels — the canonical label set used by the Kubernetes
// scheduler to assign pods to the DaemonSet.
//
// Returns an error if the DaemonSet cannot be fetched or if the pod list query
// fails.  Returns an empty slice (not an error) if the DaemonSet has no pods yet.
func dsFetchPods(ctx context.Context, c client.Client, dsName, ns string) ([]corev1.Pod, error) {
	ds := &appsv1.DaemonSet{}
	if err := c.Get(ctx, client.ObjectKey{Name: dsName, Namespace: ns}, ds); err != nil {
		return nil, fmt.Errorf("dsFetchPods: get DaemonSet %q in %q: %w", dsName, ns, err)
	}

	podList := &corev1.PodList{}
	if err := c.List(ctx, podList,
		client.InNamespace(ns),
		client.MatchingLabels(ds.Spec.Selector.MatchLabels),
	); err != nil {
		return nil, fmt.Errorf("dsFetchPods: list pods for DaemonSet %q: %w", dsName, err)
	}
	return podList.Items, nil
}

// dsPodCondition returns the PodCondition of the given type from the pod's
// status, or nil if no such condition exists.
func dsPodCondition(pod corev1.Pod, condType corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == condType {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// dsContainerStateString returns a human-readable description of a
// ContainerState for use in failure messages.
func dsContainerStateString(state corev1.ContainerState) string {
	switch {
	case state.Running != nil:
		return fmt.Sprintf("Running(since=%s)", state.Running.StartedAt.Time.Format(time.RFC3339))
	case state.Waiting != nil:
		return fmt.Sprintf("Waiting(reason=%s, message=%s)", state.Waiting.Reason, state.Waiting.Message)
	case state.Terminated != nil:
		return fmt.Sprintf("Terminated(reason=%s, exitCode=%d)", state.Terminated.Reason, state.Terminated.ExitCode)
	default:
		return "Unknown"
	}
}
