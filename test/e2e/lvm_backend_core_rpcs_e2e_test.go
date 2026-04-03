//go:build e2e

package e2e

// lvm_backend_core_rpcs_e2e_test.go — E33.1: LVM backend Core RPC tests.
//
// Tests the 6 core LVM backend RPCs against a real pillar-agent running inside
// a Kind cluster with a real LVM VG (loopback device based).
//
// Prerequisites:
//   - Kind cluster bootstrapped (KUBECONFIG set)
//   - pillar-agent DaemonSet running on storage-worker node
//   - PILLAR_E2E_LVM_VG environment variable set to the LVM VG name
//   - Optional: PILLAR_E2E_LVM_THIN_POOL for thin LV tests
//
// TC IDs covered: E33.285 – E33.293 (E33.1 subsection)
//
// Build tag: //go:build e2e
// Run with: go test -tags=e2e ./test/e2e/ --ginkgo.label-filter="lvm && rpc"

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// ─────────────────────────────────────────────────────────────────────────────
// E33.1 shared helpers
// ─────────────────────────────────────────────────────────────────────────────

const (
	e33AgentGRPCPort    = 9500
	e33AgentNamespace   = "pillar-csi-system"
	e33AgentPodSelector = "app.kubernetes.io/component=agent"
)

// e33LvmVG returns the LVM volume group name for E33 tests.
// Returns "" when PILLAR_E2E_LVM_VG is not set.
func e33LvmVG() string { return os.Getenv("PILLAR_E2E_LVM_VG") }

// e33LvmThinPool returns the thin pool name for E33 thin-LV tests.
// Returns "" when PILLAR_E2E_LVM_THIN_POOL is not set.
func e33LvmThinPool() string { return os.Getenv("PILLAR_E2E_LVM_THIN_POOL") }

// e33KubectlOutput runs kubectl with the suite kubeconfig and returns stdout.
func e33KubectlOutput(ctx context.Context, args ...string) (string, error) {
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" && suiteKindCluster != nil {
		kubeconfigPath = suiteKindCluster.KubeconfigPath
	}
	if kubeconfigPath == "" {
		return "", fmt.Errorf("[E33] KUBECONFIG not set — Kind cluster not bootstrapped")
	}
	cmdArgs := append([]string{"--kubeconfig=" + kubeconfigPath}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", cmdArgs...) //nolint:gosec
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl %s: %w\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// e33AgentPodName returns the name of the first pillar-agent pod in the
// storage-worker namespace. Returns an error if no agent pod is found.
func e33AgentPodName(ctx context.Context) (string, error) {
	out, err := e33KubectlOutput(ctx,
		"get", "pods",
		"-n", e33AgentNamespace,
		"-l", e33AgentPodSelector,
		"-o", "jsonpath={.items[0].metadata.name}",
	)
	if err != nil {
		return "", fmt.Errorf("get agent pod name: %w", err)
	}
	if out == "" {
		return "", fmt.Errorf("no agent pod found with selector %q in namespace %q",
			e33AgentPodSelector, e33AgentNamespace)
	}
	return out, nil
}

// e33PortForwardAgentGRPC starts a kubectl port-forward to the agent pod's
// gRPC port. Returns the local address "127.0.0.1:<localPort>" and a cancel
// function to stop the port-forward.
func e33PortForwardAgentGRPC(ctx context.Context, podName string, localPort int) (string, context.CancelFunc, error) {
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" && suiteKindCluster != nil {
		kubeconfigPath = suiteKindCluster.KubeconfigPath
	}

	pfCtx, cancel := context.WithCancel(ctx)

	cmd := exec.CommandContext(pfCtx, "kubectl", //nolint:gosec
		"--kubeconfig="+kubeconfigPath,
		"port-forward",
		"-n", e33AgentNamespace,
		"pod/"+podName,
		fmt.Sprintf("%d:%d", localPort, e33AgentGRPCPort),
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, fmt.Errorf("start port-forward: %w", err)
	}

	// Wait briefly for port-forward to establish.
	time.Sleep(500 * time.Millisecond)

	addr := fmt.Sprintf("127.0.0.1:%d", localPort)
	return addr, func() {
		cancel()
		_ = cmd.Wait()
	}, nil
}

// e33AgentGRPCClient creates an insecure gRPC client to the given address.
func e33AgentGRPCClient(ctx context.Context, addr string) (agentv1.AgentServiceClient, *grpc.ClientConn, error) {
	conn, err := grpc.DialContext(ctx, addr, //nolint:staticcheck
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),                 //nolint:staticcheck
		grpc.WithTimeout(10*time.Second), //nolint:staticcheck
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial agent at %s: %w", addr, err)
	}
	return agentv1.NewAgentServiceClient(conn), conn, nil
}

// e33FailIfNoInfra fails the current spec if E33 infrastructure is not available.
func e33FailIfNoInfra() {
	if e33LvmVG() == "" {
		Fail("[E33] MISSING PREREQUISITE: PILLAR_E2E_LVM_VG not set.\n" +
			"  This env var must be set to the LVM volume group name provisioned inside the Kind cluster.\n" +
			"  It is normally exported by main_test.go bootstrapSuiteBackends during suite setup.\n" +
			"  Run: export PILLAR_E2E_LVM_VG=<vg-name>  to set it manually.")
	}
	if os.Getenv("KUBECONFIG") == "" && suiteKindCluster == nil {
		Fail("[E33] MISSING PREREQUISITE: No Kind cluster available.\n" +
			"  KUBECONFIG must point to a running cluster or the Kind cluster must be bootstrapped.\n" +
			"  Run: export KUBECONFIG=<path-to-kubeconfig>  or run go test without -run to bootstrap Kind.")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E33.1: LVM 백엔드 Core RPC
// ─────────────────────────────────────────────────────────────────────────────

// Note: "default-profile" is intentionally absent here.
// E33 tests require a deployed pillar-agent pod (via Helm install) which is
// NOT part of the 2-minute default make test-e2e run. Use:
//
//	make test-e2e E2E_LABEL_FILTER="e33"  (after helm install)
var _ = Describe("E33: LVM Kind 클러스터 E2E — 실제 LVM VG + NVMe-oF TCP",
	Label("lvm", "rpc", "e33"),
	func() {
		Describe("E33.1 LVM 백엔드 Core RPC", Ordered, func() {

			var (
				agentClient agentv1.AgentServiceClient
				conn        *grpc.ClientConn
				stopPF      context.CancelFunc
				lvmVG       string
				lvmThinPool string
				// uniqueID ensures LV names don't collide across parallel test runs.
				uniqueID string
			)

			BeforeAll(func() {
				e33FailIfNoInfra()

				lvmVG = e33LvmVG()
				lvmThinPool = e33LvmThinPool()
				uniqueID = fmt.Sprintf("e33-%d", GinkgoParallelProcess())

				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				By("finding pillar-agent pod")
				podName, err := e33AgentPodName(ctx)
				Expect(err).NotTo(HaveOccurred(), "[E33.1] find agent pod")

				By("starting port-forward to agent gRPC port")
				localPort := 49500 + GinkgoParallelProcess()
				addr, stop, err := e33PortForwardAgentGRPC(ctx, podName, localPort)
				Expect(err).NotTo(HaveOccurred(), "[E33.1] port-forward setup")
				stopPF = stop

				By("connecting gRPC client")
				var dialCtx context.Context
				var dialCancel context.CancelFunc
				dialCtx, dialCancel = context.WithTimeout(context.Background(), 15*time.Second)
				defer dialCancel()
				agentClient, conn, err = e33AgentGRPCClient(dialCtx, addr)
				Expect(err).NotTo(HaveOccurred(), "[E33.1] gRPC client dial")
			})

			AfterAll(func() {
				if conn != nil {
					_ = conn.Close()
				}
				if stopPF != nil {
					stopPF()
				}
			})

			// ── TC-E33.285 ────────────────────────────────────────────────────
			It("[TC-E33.285] GetCapacity returns positive total and available bytes for the LVM VG", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				resp, err := agentClient.GetCapacity(ctx, &agentv1.GetCapacityRequest{
					BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
					PoolName:    lvmVG,
				})
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E33.285] GetCapacity must succeed for VG %q", lvmVG)
				Expect(resp.GetTotalBytes()).To(BeNumerically(">", 0),
					"[TC-E33.285] TotalBytes must be positive")
				Expect(resp.GetAvailableBytes()).To(BeNumerically(">", 0),
					"[TC-E33.285] AvailableBytes must be positive")
				Expect(resp.GetAvailableBytes()).To(BeNumerically("<=", resp.GetTotalBytes()),
					"[TC-E33.285] AvailableBytes must not exceed TotalBytes")
			})

			// ── TC-E33.286 ────────────────────────────────────────────────────
			It("[TC-E33.286] CreateVolume (thin) returns device_path=/dev/<vg>/<lv>", func() {
				if lvmThinPool == "" {
					Fail("[TC-E33.286] MISSING PREREQUISITE: PILLAR_E2E_LVM_THIN_POOL not set — skipping thin LV test")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				volumeID := fmt.Sprintf("%s/e33-286-%s", lvmVG, uniqueID)
				resp, err := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
					VolumeId:      volumeID,
					CapacityBytes: 512 * 1024 * 1024, // 512 MiB
					BackendParams: &agentv1.BackendParams{
						Params: &agentv1.BackendParams_Lvm{
							Lvm: &agentv1.LvmVolumeParams{
								VolumeGroup:   lvmVG,
								ProvisionMode: "thin",
							},
						},
					},
				})
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E33.286] CreateVolume (thin) must succeed")
				expectedPath := fmt.Sprintf("/dev/%s/e33-286-%s", lvmVG, uniqueID)
				Expect(resp.GetDevicePath()).To(Equal(expectedPath),
					"[TC-E33.286] device_path must be /dev/<vg>/<lv>")
				Expect(resp.GetCapacityBytes()).To(BeNumerically(">=", int64(512*1024*1024)),
					"[TC-E33.286] capacity_bytes must be >= requested size")

				// Cleanup
				DeferCleanup(func() {
					cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					_, _ = agentClient.DeleteVolume(cleanCtx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
				})
			})

			// ── TC-E33.287 ────────────────────────────────────────────────────
			It("[TC-E33.287] CreateVolume (linear) creates a linear LV using ProvisionMode override", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				volumeID := fmt.Sprintf("%s/e33-287-%s", lvmVG, uniqueID)
				resp, err := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
					VolumeId:      volumeID,
					CapacityBytes: 256 * 1024 * 1024, // 256 MiB
					BackendParams: &agentv1.BackendParams{
						Params: &agentv1.BackendParams_Lvm{
							Lvm: &agentv1.LvmVolumeParams{
								VolumeGroup:   lvmVG,
								ProvisionMode: "linear",
							},
						},
					},
				})
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E33.287] CreateVolume (linear) must succeed")
				Expect(resp.GetDevicePath()).To(MatchRegexp(`^/dev/[^/]+/[^/]+$`),
					"[TC-E33.287] device_path must be /dev/<vg>/<lv> format")

				DeferCleanup(func() {
					cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					_, _ = agentClient.DeleteVolume(cleanCtx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
				})
			})

			// ── TC-E33.288 ────────────────────────────────────────────────────
			It("[TC-E33.288] DeleteVolume destroys an LV and is idempotent", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				// First create an LV to delete.
				volumeID := fmt.Sprintf("%s/e33-288-%s", lvmVG, uniqueID)
				_, err := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
					VolumeId:      volumeID,
					CapacityBytes: 256 * 1024 * 1024,
					BackendParams: &agentv1.BackendParams{
						Params: &agentv1.BackendParams_Lvm{
							Lvm: &agentv1.LvmVolumeParams{
								VolumeGroup:   lvmVG,
								ProvisionMode: "linear",
							},
						},
					},
				})
				Expect(err).NotTo(HaveOccurred(), "[TC-E33.288] create LV for delete test")

				// First delete.
				_, err = agentClient.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E33.288] first DeleteVolume must succeed")

				// Second delete (idempotent).
				_, err = agentClient.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E33.288] second DeleteVolume (idempotent) must succeed")
			})

			// ── TC-E33.289 ────────────────────────────────────────────────────
			It("[TC-E33.289] ExpandVolume grows an LVM LV to at least the requested size", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				volumeID := fmt.Sprintf("%s/e33-289-%s", lvmVG, uniqueID)

				By("creating initial 512 MiB LV")
				_, err := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
					VolumeId:      volumeID,
					CapacityBytes: 512 * 1024 * 1024,
					BackendParams: &agentv1.BackendParams{
						Params: &agentv1.BackendParams_Lvm{
							Lvm: &agentv1.LvmVolumeParams{
								VolumeGroup:   lvmVG,
								ProvisionMode: "linear",
							},
						},
					},
				})
				Expect(err).NotTo(HaveOccurred(), "[TC-E33.289] create initial LV")

				By("expanding to 1 GiB")
				expandResp, err := agentClient.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
					VolumeId:       volumeID,
					RequestedBytes: 1024 * 1024 * 1024,
				})
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E33.289] ExpandVolume must succeed")
				Expect(expandResp.GetCapacityBytes()).To(BeNumerically(">=", int64(1024*1024*1024)),
					"[TC-E33.289] capacity_bytes must be >= 1 GiB after expansion")

				DeferCleanup(func() {
					cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					_, _ = agentClient.DeleteVolume(cleanCtx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
				})
			})

			// ── TC-E33.290 ────────────────────────────────────────────────────
			It("[TC-E33.290] ListVolumes returns created LVs with correct device_path", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				volumeID := fmt.Sprintf("%s/e33-290-%s", lvmVG, uniqueID)

				By("creating an LV")
				_, err := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
					VolumeId:      volumeID,
					CapacityBytes: 256 * 1024 * 1024,
					BackendParams: &agentv1.BackendParams{
						Params: &agentv1.BackendParams_Lvm{
							Lvm: &agentv1.LvmVolumeParams{
								VolumeGroup:   lvmVG,
								ProvisionMode: "linear",
							},
						},
					},
				})
				Expect(err).NotTo(HaveOccurred(), "[TC-E33.290] create LV for list test")

				DeferCleanup(func() {
					cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					_, _ = agentClient.DeleteVolume(cleanCtx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
				})

				By("listing volumes")
				listResp, err := agentClient.ListVolumes(ctx, &agentv1.ListVolumesRequest{
					BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
					PoolName:    lvmVG,
				})
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E33.290] ListVolumes must succeed")

				// Find our volume in the list.
				var found bool
				for _, vol := range listResp.GetVolumes() {
					if vol.GetVolumeId() == volumeID {
						found = true
						Expect(vol.GetDevicePath()).To(MatchRegexp(`^/dev/[^/]+/[^/]+$`),
							"[TC-E33.290] device_path must be /dev/<vg>/<lv> format")
						break
					}
				}
				Expect(found).To(BeTrue(),
					"[TC-E33.290] created LV %q must appear in ListVolumes response", volumeID)
			})

			// ── TC-E33.291 ────────────────────────────────────────────────────
			It("[TC-E33.291] CreateVolume is idempotent: re-creating with same volume ID succeeds (linear)", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				volumeID := fmt.Sprintf("%s/e33-291-%s", lvmVG, uniqueID)
				req := &agentv1.CreateVolumeRequest{
					VolumeId:      volumeID,
					CapacityBytes: 256 * 1024 * 1024,
					BackendParams: &agentv1.BackendParams{
						Params: &agentv1.BackendParams_Lvm{
							Lvm: &agentv1.LvmVolumeParams{
								VolumeGroup:   lvmVG,
								ProvisionMode: "linear",
							},
						},
					},
				}

				resp1, err := agentClient.CreateVolume(ctx, req)
				Expect(err).NotTo(HaveOccurred(), "[TC-E33.291] first CreateVolume")

				resp2, err := agentClient.CreateVolume(ctx, req)
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E33.291] idempotent CreateVolume must succeed")
				Expect(resp2.GetDevicePath()).To(Equal(resp1.GetDevicePath()),
					"[TC-E33.291] idempotent call must return same device_path")

				DeferCleanup(func() {
					cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					_, _ = agentClient.DeleteVolume(cleanCtx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
				})
			})

			// ── TC-E33.292 ────────────────────────────────────────────────────
			It("[TC-E33.292] CreateVolume is idempotent: re-creating with same volume ID succeeds (thin)", func() {
				if lvmThinPool == "" {
					Fail("[TC-E33.292] MISSING PREREQUISITE: PILLAR_E2E_LVM_THIN_POOL not set — skipping thin LV idempotency test")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				volumeID := fmt.Sprintf("%s/e33-292-%s", lvmVG, uniqueID)
				req := &agentv1.CreateVolumeRequest{
					VolumeId:      volumeID,
					CapacityBytes: 256 * 1024 * 1024,
					BackendParams: &agentv1.BackendParams{
						Params: &agentv1.BackendParams_Lvm{
							Lvm: &agentv1.LvmVolumeParams{
								VolumeGroup:   lvmVG,
								ProvisionMode: "thin",
							},
						},
					},
				}

				resp1, err := agentClient.CreateVolume(ctx, req)
				Expect(err).NotTo(HaveOccurred(), "[TC-E33.292] first thin CreateVolume")

				resp2, err := agentClient.CreateVolume(ctx, req)
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E33.292] idempotent thin CreateVolume must succeed")
				Expect(resp2.GetDevicePath()).To(Equal(resp1.GetDevicePath()),
					"[TC-E33.292] idempotent thin call must return same device_path")

				DeferCleanup(func() {
					cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					_, _ = agentClient.DeleteVolume(cleanCtx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
				})
			})

			// ── TC-E33.293 ────────────────────────────────────────────────────
			It("[TC-E33.293] returns an error for a non-existent LVM VG pool name", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				_, err := agentClient.GetCapacity(ctx, &agentv1.GetCapacityRequest{
					BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
					PoolName:    "nonexistent-vg-e33",
				})
				Expect(err).To(HaveOccurred(),
					"[TC-E33.293] GetCapacity for non-existent VG must return an error")
			})

		})
	})
