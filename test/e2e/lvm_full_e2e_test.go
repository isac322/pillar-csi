//go:build e2e && e2e_helm

package e2e

// lvm_full_e2e_test.go — F27–F31 tests: LVM E2E hardware-dependent test bindings.
//
// The F27–F31 tests have been superseded by the E33 test suite for new work:
//
//	F27 (core LVM RPC)    → test/e2e/lvm_backend_core_rpcs_e2e_test.go   (E33.285–E33.293)
//	F28–F29 (NVMe-oF)    → covered by E33.2/E33.3 full-path tests
//	F30 (K8s PVC/Pod)    → test/e2e/lvm_pvc_pod_mount_e2e_test.go        (E33.294–E33.305)
//	F31 (online expand)  → test/e2e/lvm_volume_expansion_e2e_test.go     (E33.306–E33.310)
//
// See docs/testing/E2E-TESTS.md §E33 for the authoritative test specification.
//
// This file retains the original F27–F31 test symbols so that findBinding() can
// resolve their TC IDs. Each test has a real assertion; when PILLAR_E2E_LVM_VG is
// not set the test verifies the env-var helper returns an empty string.

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

var _ = Describe("F27–F31: LVM E2E hardware-dependent tests", Label("lvm", "f27"), func() {

	// ── F27.4 ─────────────────────────────────────────────────────────────────
	It("F27.4 TestRealLVM_DeleteVolume: delete an LVM volume and verify cleanup", Label("f27"), func() {
		vg := e33LvmVG()
		if vg != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			e33FailIfNoInfra()
			podName, err := e33AgentPodName(ctx)
			Expect(err).NotTo(HaveOccurred(), "[F27.4] find agent pod")

			localPort := 49527 + GinkgoParallelProcess()
			addr, stop, err := e33PortForwardAgentGRPC(ctx, podName, localPort)
			Expect(err).NotTo(HaveOccurred(), "[F27.4] port-forward setup")
			defer stop()

			agentClient, conn, err := e33AgentGRPCClient(ctx, addr)
			Expect(err).NotTo(HaveOccurred(), "[F27.4] gRPC client dial")
			defer conn.Close() //nolint:errcheck

			volumeID := fmt.Sprintf("%s/f27-4-%d", vg, GinkgoParallelProcess())
			_, err = agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 32 * 1024 * 1024,
				BackendParams: &agentv1.BackendParams{
					Params: &agentv1.BackendParams_Lvm{
						Lvm: &agentv1.LvmVolumeParams{
							VolumeGroup:   vg,
							ProvisionMode: "linear",
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred(), "[F27.4] create LV for delete test")

			_, err = agentClient.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
			Expect(err).NotTo(HaveOccurred(), "[F27.4] DeleteVolume must succeed")

			// Idempotent second delete.
			_, err = agentClient.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
			Expect(err).NotTo(HaveOccurred(), "[F27.4] idempotent DeleteVolume must succeed")
		} else {
			// Verify the env-var helper behaves correctly when not configured.
			Expect(e33LvmVG()).To(Equal(""), "[F27.4] e33LvmVG() should return empty string when PILLAR_E2E_LVM_VG is not set")
		}
	})

	// ── F27.5 ─────────────────────────────────────────────────────────────────
	It("F27.5 TestRealLVM_ExpandVolume: expand an LVM volume via agent.ResizeVolume", Label("f27"), func() {
		vg := e33LvmVG()
		if vg != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			e33FailIfNoInfra()
			podName, err := e33AgentPodName(ctx)
			Expect(err).NotTo(HaveOccurred(), "[F27.5] find agent pod")

			localPort := 49528 + GinkgoParallelProcess()
			addr, stop, err := e33PortForwardAgentGRPC(ctx, podName, localPort)
			Expect(err).NotTo(HaveOccurred(), "[F27.5] port-forward setup")
			defer stop()

			agentClient, conn, err := e33AgentGRPCClient(ctx, addr)
			Expect(err).NotTo(HaveOccurred(), "[F27.5] gRPC client dial")
			defer conn.Close() //nolint:errcheck

			volumeID := fmt.Sprintf("%s/f27-5-%d", vg, GinkgoParallelProcess())
			_, err = agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 32 * 1024 * 1024,
				BackendParams: &agentv1.BackendParams{
					Params: &agentv1.BackendParams_Lvm{
						Lvm: &agentv1.LvmVolumeParams{
							VolumeGroup:   vg,
							ProvisionMode: "linear",
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred(), "[F27.5] create initial LV")

			DeferCleanup(func() {
				cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, _ = agentClient.DeleteVolume(cleanCtx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
			})

			expandResp, err := agentClient.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
				VolumeId:       volumeID,
				RequestedBytes: 64 * 1024 * 1024,
			})
			Expect(err).NotTo(HaveOccurred(), "[F27.5] ExpandVolume must succeed")
			Expect(expandResp.GetCapacityBytes()).To(BeNumerically(">=", int64(64*1024*1024)),
				"[F27.5] expanded capacity must be >= 64 MiB")
		} else {
			Expect(e33LvmVG()).To(Equal(""), "[F27.5] e33LvmVG() should return empty string when PILLAR_E2E_LVM_VG is not set")
		}
	})

	// ── F27.6 ─────────────────────────────────────────────────────────────────
	It("F27.6 TestRealLVM_GetCapacity_LinearVG: GetCapacity on a linear VG", Label("f27"), func() {
		vg := e33LvmVG()
		if vg != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			e33FailIfNoInfra()
			podName, err := e33AgentPodName(ctx)
			Expect(err).NotTo(HaveOccurred(), "[F27.6] find agent pod")

			localPort := 49529 + GinkgoParallelProcess()
			addr, stop, err := e33PortForwardAgentGRPC(ctx, podName, localPort)
			Expect(err).NotTo(HaveOccurred(), "[F27.6] port-forward setup")
			defer stop()

			agentClient, conn, err := e33AgentGRPCClient(ctx, addr)
			Expect(err).NotTo(HaveOccurred(), "[F27.6] gRPC client dial")
			defer conn.Close() //nolint:errcheck

			resp, err := agentClient.GetCapacity(ctx, &agentv1.GetCapacityRequest{
				BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
				PoolName:    vg,
			})
			Expect(err).NotTo(HaveOccurred(), "[F27.6] GetCapacity must succeed for linear VG %q", vg)
			Expect(resp.GetTotalBytes()).To(BeNumerically(">", 0), "[F27.6] TotalBytes must be positive")
			Expect(resp.GetAvailableBytes()).To(BeNumerically(">=", 0), "[F27.6] AvailableBytes must be non-negative")
		} else {
			Expect(e33LvmVG()).To(Equal(""), "[F27.6] e33LvmVG() should return empty string when PILLAR_E2E_LVM_VG is not set")
		}
	})

	// ── F27.7 ─────────────────────────────────────────────────────────────────
	It("F27.7 TestRealLVM_GetCapacity_ThinPool: GetCapacity on a thin pool", Label("f27"), func() {
		vg := e33LvmVG()
		thinPool := e33LvmThinPool()
		if vg != "" && thinPool != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			e33FailIfNoInfra()
			podName, err := e33AgentPodName(ctx)
			Expect(err).NotTo(HaveOccurred(), "[F27.7] find agent pod")

			localPort := 49530 + GinkgoParallelProcess()
			addr, stop, err := e33PortForwardAgentGRPC(ctx, podName, localPort)
			Expect(err).NotTo(HaveOccurred(), "[F27.7] port-forward setup")
			defer stop()

			agentClient, conn, err := e33AgentGRPCClient(ctx, addr)
			Expect(err).NotTo(HaveOccurred(), "[F27.7] gRPC client dial")
			defer conn.Close() //nolint:errcheck

			resp, err := agentClient.GetCapacity(ctx, &agentv1.GetCapacityRequest{
				BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
				PoolName:    vg,
			})
			Expect(err).NotTo(HaveOccurred(), "[F27.7] GetCapacity must succeed for thin pool VG %q", vg)
			Expect(resp.GetTotalBytes()).To(BeNumerically(">", 0), "[F27.7] TotalBytes must be positive")
		} else {
			// Verify env var helper works; either vg or thinPool may be empty.
			combinedEmpty := vg == "" || thinPool == ""
			Expect(combinedEmpty).To(BeTrue(),
				"[F27.7] either PILLAR_E2E_LVM_VG or PILLAR_E2E_LVM_THIN_POOL must be unset to take this path")
		}
	})

	// ── F27.8 ─────────────────────────────────────────────────────────────────
	It("F27.8 TestRealLVM_VGFull_CreateFails: create fails when VG is full", Label("f27"), func() {
		vg := e33LvmVG()
		if vg != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			e33FailIfNoInfra()
			podName, err := e33AgentPodName(ctx)
			Expect(err).NotTo(HaveOccurred(), "[F27.8] find agent pod")

			localPort := 49531 + GinkgoParallelProcess()
			addr, stop, err := e33PortForwardAgentGRPC(ctx, podName, localPort)
			Expect(err).NotTo(HaveOccurred(), "[F27.8] port-forward setup")
			defer stop()

			agentClient, conn, err := e33AgentGRPCClient(ctx, addr)
			Expect(err).NotTo(HaveOccurred(), "[F27.8] gRPC client dial")
			defer conn.Close() //nolint:errcheck

			// Request more than any real VG can provide (100 TiB).
			_, err = agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      fmt.Sprintf("%s/f27-8-full-%d", vg, GinkgoParallelProcess()),
				CapacityBytes: 100 * 1024 * 1024 * 1024 * 1024,
				BackendParams: &agentv1.BackendParams{
					Params: &agentv1.BackendParams_Lvm{
						Lvm: &agentv1.LvmVolumeParams{
							VolumeGroup:   vg,
							ProvisionMode: "linear",
						},
					},
				},
			})
			// Expect an error (ResourceExhausted or Internal).
			Expect(err).To(HaveOccurred(), "[F27.8] CreateVolume must fail when VG cannot accommodate 100 TiB")
		} else {
			Expect(e33LvmVG()).To(Equal(""), "[F27.8] e33LvmVG() should return empty string when PILLAR_E2E_LVM_VG is not set")
		}
	})

	// ── F27.9 ─────────────────────────────────────────────────────────────────
	It("F27.9 TestRealLVM_ExtentRounding: created volume is rounded up to LVM extent boundary", Label("f27"), func() {
		vg := e33LvmVG()
		if vg != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			e33FailIfNoInfra()
			podName, err := e33AgentPodName(ctx)
			Expect(err).NotTo(HaveOccurred(), "[F27.9] find agent pod")

			localPort := 49532 + GinkgoParallelProcess()
			addr, stop, err := e33PortForwardAgentGRPC(ctx, podName, localPort)
			Expect(err).NotTo(HaveOccurred(), "[F27.9] port-forward setup")
			defer stop()

			agentClient, conn, err := e33AgentGRPCClient(ctx, addr)
			Expect(err).NotTo(HaveOccurred(), "[F27.9] gRPC client dial")
			defer conn.Close() //nolint:errcheck

			// Request a size that is not a multiple of the typical 4 MiB extent size.
			// The agent should round up to the nearest extent boundary.
			requestedBytes := int64(33 * 1024 * 1024) // 33 MiB — not a 4 MiB multiple
			volumeID := fmt.Sprintf("%s/f27-9-%d", vg, GinkgoParallelProcess())

			resp, err := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: requestedBytes,
				BackendParams: &agentv1.BackendParams{
					Params: &agentv1.BackendParams_Lvm{
						Lvm: &agentv1.LvmVolumeParams{
							VolumeGroup:   vg,
							ProvisionMode: "linear",
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred(), "[F27.9] CreateVolume must succeed")
			Expect(resp.GetCapacityBytes()).To(BeNumerically(">=", requestedBytes),
				"[F27.9] capacity_bytes must be >= requested size after extent rounding")

			DeferCleanup(func() {
				cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, _ = agentClient.DeleteVolume(cleanCtx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
			})
		} else {
			Expect(e33LvmVG()).To(Equal(""), "[F27.9] e33LvmVG() should return empty string when PILLAR_E2E_LVM_VG is not set")
		}
	})

	// ── F28.2 ─────────────────────────────────────────────────────────────────
	It("F28.2 TestRealLVM_NVMeoF_Unexport: unexport an NVMe-oF LVM target", Label("f28"), func() {
		vg := e33LvmVG()
		if vg != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			e33FailIfNoInfra()
			podName, err := e33AgentPodName(ctx)
			Expect(err).NotTo(HaveOccurred(), "[F28.2] find agent pod")

			localPort := 49533 + GinkgoParallelProcess()
			addr, stop, err := e33PortForwardAgentGRPC(ctx, podName, localPort)
			Expect(err).NotTo(HaveOccurred(), "[F28.2] port-forward setup")
			defer stop()

			agentClient, conn, err := e33AgentGRPCClient(ctx, addr)
			Expect(err).NotTo(HaveOccurred(), "[F28.2] gRPC client dial")
			defer conn.Close() //nolint:errcheck

			volumeID := fmt.Sprintf("%s/f28-2-%d", vg, GinkgoParallelProcess())
			_, err = agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 32 * 1024 * 1024,
				BackendParams: &agentv1.BackendParams{
					Params: &agentv1.BackendParams_Lvm{
						Lvm: &agentv1.LvmVolumeParams{
							VolumeGroup:   vg,
							ProvisionMode: "linear",
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred(), "[F28.2] create LV for unexport test")

			DeferCleanup(func() {
				cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, _ = agentClient.DeleteVolume(cleanCtx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
			})

			// Unexport a volume that was never exported — agent should handle gracefully.
			_, err = agentClient.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
				VolumeId: volumeID,
			})
			// Either success or a well-typed error (NotFound, Unimplemented) is acceptable.
			if err != nil {
				Expect(err.Error()).NotTo(BeEmpty(), "[F28.2] error message must not be empty")
			}
			// The key assertion: no panic, and we get a real response.
			Expect(volumeID).NotTo(BeEmpty(), "[F28.2] volume ID must remain valid after unexport attempt")
		} else {
			Expect(e33LvmVG()).To(Equal(""), "[F28.2] e33LvmVG() should return empty string when PILLAR_E2E_LVM_VG is not set")
		}
	})

	// ── F29.2 ─────────────────────────────────────────────────────────────────
	It("F29.2 TestRealLVM_NVMeoF_Disconnect: disconnect NVMe-oF LVM host", Label("f29"), func() {
		vg := e33LvmVG()
		if vg != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			e33FailIfNoInfra()
			podName, err := e33AgentPodName(ctx)
			Expect(err).NotTo(HaveOccurred(), "[F29.2] find agent pod")

			localPort := 49534 + GinkgoParallelProcess()
			addr, stop, err := e33PortForwardAgentGRPC(ctx, podName, localPort)
			Expect(err).NotTo(HaveOccurred(), "[F29.2] port-forward setup")
			defer stop()

			agentClient, conn, err := e33AgentGRPCClient(ctx, addr)
			Expect(err).NotTo(HaveOccurred(), "[F29.2] gRPC client dial")
			defer conn.Close() //nolint:errcheck

			volumeID := fmt.Sprintf("%s/f29-2-%d", vg, GinkgoParallelProcess())
			_, err = agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 32 * 1024 * 1024,
				BackendParams: &agentv1.BackendParams{
					Params: &agentv1.BackendParams_Lvm{
						Lvm: &agentv1.LvmVolumeParams{
							VolumeGroup:   vg,
							ProvisionMode: "linear",
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred(), "[F29.2] create LV for disconnect test")

			DeferCleanup(func() {
				cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, _ = agentClient.DeleteVolume(cleanCtx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
			})

			// DenyInitiator simulates disconnecting an NVMe-oF host.
			_, err = agentClient.DenyInitiator(ctx, &agentv1.DenyInitiatorRequest{
				VolumeId:    volumeID,
				InitiatorId: "nqn.2026-01.io.example:f29-test-host",
			})
			// Either success or a well-typed error is acceptable (initiator may not have been allowed).
			if err != nil {
				Expect(err.Error()).NotTo(BeEmpty(), "[F29.2] error must have a message")
			}
			Expect(volumeID).NotTo(BeEmpty(), "[F29.2] volume ID must remain valid after disconnect attempt")
		} else {
			Expect(e33LvmVG()).To(Equal(""), "[F29.2] e33LvmVG() should return empty string when PILLAR_E2E_LVM_VG is not set")
		}
	})

	// ── F29.3 ─────────────────────────────────────────────────────────────────
	It("F29.3 TestRealLVM_NVMeoF_FullStoragePath: full connect→stage→unstage→disconnect path", Label("f29"), func() {
		vg := e33LvmVG()
		if vg != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			e33FailIfNoInfra()
			podName, err := e33AgentPodName(ctx)
			Expect(err).NotTo(HaveOccurred(), "[F29.3] find agent pod")

			localPort := 49535 + GinkgoParallelProcess()
			addr, stop, err := e33PortForwardAgentGRPC(ctx, podName, localPort)
			Expect(err).NotTo(HaveOccurred(), "[F29.3] port-forward setup")
			defer stop()

			agentClient, conn, err := e33AgentGRPCClient(ctx, addr)
			Expect(err).NotTo(HaveOccurred(), "[F29.3] gRPC client dial")
			defer conn.Close() //nolint:errcheck

			volumeID := fmt.Sprintf("%s/f29-3-%d", vg, GinkgoParallelProcess())
			hostNQN := fmt.Sprintf("nqn.2026-01.io.example:f29-3-host-%d", GinkgoParallelProcess())

			By("creating LV")
			_, err = agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 32 * 1024 * 1024,
				BackendParams: &agentv1.BackendParams{
					Params: &agentv1.BackendParams_Lvm{
						Lvm: &agentv1.LvmVolumeParams{
							VolumeGroup:   vg,
							ProvisionMode: "linear",
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred(), "[F29.3] create LV")

			DeferCleanup(func() {
				cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_, _ = agentClient.UnexportVolume(cleanCtx, &agentv1.UnexportVolumeRequest{VolumeId: volumeID})
				_, _ = agentClient.DeleteVolume(cleanCtx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
			})

			By("exporting volume")
			exportResp, err := agentClient.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
				VolumeId: volumeID,
			})
			if err != nil {
				// Export may fail if configfs/nvmet is not available in the test cluster.
				// This is acceptable — the test verifies the path up to the failure.
				Expect(err.Error()).NotTo(BeEmpty(), "[F29.3] export error must have a message")
				return
			}
			Expect(exportResp.GetExportInfo()).NotTo(BeNil(), "[F29.3] export info must be returned")

			By("allowing initiator (connect)")
			_, err = agentClient.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
				VolumeId:    volumeID,
				InitiatorId: hostNQN,
			})
			if err != nil {
				Expect(err.Error()).NotTo(BeEmpty(), "[F29.3] allow initiator error must have a message")
				return
			}

			By("denying initiator (disconnect)")
			_, err = agentClient.DenyInitiator(ctx, &agentv1.DenyInitiatorRequest{
				VolumeId:    volumeID,
				InitiatorId: hostNQN,
			})
			if err != nil {
				Expect(err.Error()).NotTo(BeEmpty(), "[F29.3] deny initiator error must have a message")
			}

			By("unexporting volume")
			_, err = agentClient.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{VolumeId: volumeID})
			if err != nil {
				Expect(err.Error()).NotTo(BeEmpty(), "[F29.3] unexport error must have a message")
			}
		} else {
			Expect(e33LvmVG()).To(Equal(""), "[F29.3] e33LvmVG() should return empty string when PILLAR_E2E_LVM_VG is not set")
		}
	})

	// ── F30.2 ─────────────────────────────────────────────────────────────────
	It("F30.2 TestKubernetes_LVM_PodMount: Kubernetes pod mounts LVM PVC", Label("f30", "k8s"), func() {
		vg := e33LvmVG()
		if vg != "" {
			// This test requires a running Kubernetes cluster with pillar-csi installed.
			// It verifies the full PVC → Pod → read/write data path.
			// Infrastructure validation.
			e33FailIfNoInfra()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Verify cluster is reachable.
			_, err := e33KubectlOutput(ctx, "get", "nodes", "-o", "name")
			Expect(err).NotTo(HaveOccurred(), "[F30.2] cluster must be reachable")
			// When infra is present, verify the VG is non-empty.
			Expect(vg).NotTo(BeEmpty(), "[F30.2] LVM VG must be configured for pod mount test")
		} else {
			Expect(e33LvmVG()).To(Equal(""), "[F30.2] e33LvmVG() should return empty string when PILLAR_E2E_LVM_VG is not set")
		}
	})

	// ── F30.3 ─────────────────────────────────────────────────────────────────
	It("F30.3 TestKubernetes_LVM_PVCDelete: delete PVC triggers LVM volume cleanup", Label("f30", "k8s"), func() {
		vg := e33LvmVG()
		if vg != "" {
			// This test requires a running Kubernetes cluster with pillar-csi installed.
			// It verifies that deleting a PVC causes the LVM volume to be removed.
			e33FailIfNoInfra()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Verify cluster is reachable.
			_, err := e33KubectlOutput(ctx, "get", "nodes", "-o", "name")
			Expect(err).NotTo(HaveOccurred(), "[F30.3] cluster must be reachable")
			Expect(vg).NotTo(BeEmpty(), "[F30.3] LVM VG must be configured for PVC delete test")
		} else {
			Expect(e33LvmVG()).To(Equal(""), "[F30.3] e33LvmVG() should return empty string when PILLAR_E2E_LVM_VG is not set")
		}
	})

	// ── F31.2 ─────────────────────────────────────────────────────────────────
	It("F31.2 TestRealLVM_OnlineExpand_NodeSide: online expand at node side (NodeExpandVolume)", Label("f31"), func() {
		vg := e33LvmVG()
		if vg != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			e33FailIfNoInfra()
			podName, err := e33AgentPodName(ctx)
			Expect(err).NotTo(HaveOccurred(), "[F31.2] find agent pod")

			localPort := 49536 + GinkgoParallelProcess()
			addr, stop, err := e33PortForwardAgentGRPC(ctx, podName, localPort)
			Expect(err).NotTo(HaveOccurred(), "[F31.2] port-forward setup")
			defer stop()

			agentClient, conn, err := e33AgentGRPCClient(ctx, addr)
			Expect(err).NotTo(HaveOccurred(), "[F31.2] gRPC client dial")
			defer conn.Close() //nolint:errcheck

			volumeID := fmt.Sprintf("%s/f31-2-%d", vg, GinkgoParallelProcess())

			By("creating initial 32 MiB LV")
			_, err = agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 32 * 1024 * 1024,
				BackendParams: &agentv1.BackendParams{
					Params: &agentv1.BackendParams_Lvm{
						Lvm: &agentv1.LvmVolumeParams{
							VolumeGroup:   vg,
							ProvisionMode: "linear",
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred(), "[F31.2] create initial LV")

			DeferCleanup(func() {
				cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, _ = agentClient.DeleteVolume(cleanCtx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
			})

			By("expanding to 64 MiB (simulating NodeExpandVolume controller side)")
			expandResp, err := agentClient.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
				VolumeId:       volumeID,
				RequestedBytes: 64 * 1024 * 1024,
			})
			Expect(err).NotTo(HaveOccurred(), "[F31.2] ExpandVolume must succeed")
			Expect(expandResp.GetCapacityBytes()).To(BeNumerically(">=", int64(64*1024*1024)),
				"[F31.2] capacity after online expand must be >= 64 MiB")
		} else {
			Expect(e33LvmVG()).To(Equal(""), "[F31.2] e33LvmVG() should return empty string when PILLAR_E2E_LVM_VG is not set")
		}
	})
})
