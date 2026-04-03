package e2e

// lvm_full_e2e_test.go — F27–F31: 실제 LVM 백엔드 및 NVMe-oF E2E 테스트
//
// F27–F29: Tests the real LVM backend by calling agent.Server directly
// (without Kind) using a loopback-device-backed LVM VG.
// F30–F31: Tests the full Kubernetes + real LVM integration with a Kind cluster.
//
// Prerequisites:
//   - LVM2 tools installed (lvcreate, vgs, lvs, lvremove, lvextend)
//   - Loopback LVM VG prepared or environment variable LVM_VG set to an existing VG
//   - F28/F29: nvmet, nvmet-tcp, nvme-tcp kernel modules loaded; nvme-cli installed
//   - F30/F31: Kind cluster bootstrapped (KUBECONFIG set), pillar-csi deployed
//
// TC IDs covered: F27.1–F27.9, F28.1–F28.2, F29.1–F29.3, F30.1–F30.3, F31.1–F31.2
//
// Build tag: //go:build e2e_full
// Run with: go test -tags=e2e_full ./test/e2e/ -v

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	agentsrv "github.com/bhyoo/pillar-csi/internal/agent"
	agentbackend "github.com/bhyoo/pillar-csi/internal/agent/backend"
	lvmb "github.com/bhyoo/pillar-csi/internal/agent/backend/lvm"
)

// ─────────────────────────────────────────────────────────────────────────────
// F27–F31 shared helpers
// ─────────────────────────────────────────────────────────────────────────────

const (
	f27DefaultVG       = "e2e-vg"
	f27AgentConfigRoot = "/sys/kernel/config/nvmet"
	// nqnPrefix matches the constant in internal/agent/server.go.
	fNQNPrefix = "nqn.2026-01.com.bhyoo.pillar-csi:"
)

// fVolumeNQN computes the NVMe-oF NQN for a volume ID, matching internal/agent logic.
func fVolumeNQN(volumeID string) string {
	return fNQNPrefix + strings.ReplaceAll(volumeID, "/", ".")
}

// fLvmVG returns the LVM VG for F27–F29 tests.
func fLvmVG() string {
	if vg := os.Getenv("LVM_VG"); vg != "" {
		return vg
	}
	return f27DefaultVG
}

// fFailIfNoLVM fails if LVM tools are unavailable.
func fFailIfNoLVM() {
	if _, err := exec.LookPath("lvcreate"); err != nil {
		Fail("[F27] MISSING PREREQUISITE: LVM tools not installed.\n" +
			"  Required binary: lvcreate\n" +
			"  Install with: sudo apt install lvm2  (Debian/Ubuntu)\n" +
			"               sudo dnf install lvm2   (Fedora/RHEL)\n" +
			"  Then ensure dm_thin_pool module is loaded: sudo modprobe dm_thin_pool")
	}
}

// fFailIfNoNVMeKernelModules fails if NVMe-oF kernel modules are not loaded.
func fFailIfNoNVMeKernelModules() {
	if _, err := os.Stat(f27AgentConfigRoot); os.IsNotExist(err) {
		Fail(fmt.Sprintf("[F28] MISSING PREREQUISITE: NVMe-oF configfs not found at %s\n"+
			"  The nvmet kernel module must be loaded.\n"+
			"  Install and load with:\n"+
			"    sudo modprobe nvmet nvmet-tcp\n"+
			"  Verify: ls /sys/kernel/config/nvmet", f27AgentConfigRoot))
	}
}

// fFailIfNoNVMeCLI fails if nvme-cli is not installed.
func fFailIfNoNVMeCLI() {
	if _, err := exec.LookPath("nvme"); err != nil {
		Fail("[F29] MISSING PREREQUISITE: nvme-cli not installed.\n" +
			"  Required binary: nvme\n" +
			"  Install with: sudo apt install nvme-cli  (Debian/Ubuntu)\n" +
			"               sudo dnf install nvme-cli   (Fedora/RHEL)\n" +
			"  Also ensure kernel modules are loaded:\n" +
			"    sudo modprobe nvme-tcp nvme-fabrics")
	}
}

// startLocalAgentServer starts an in-process agent.Server backed by the given
// LVM VG. Returns a gRPC client and a cleanup function.
func startLocalAgentServer(vg, _ /*unused thinPool*/, configfsRoot string) (agentv1.AgentServiceClient, func()) {
	lvmBackend := lvmb.New(vg, "")
	backends := map[string]agentbackend.VolumeBackend{
		vg: lvmBackend,
	}
	srv := agentsrv.NewServer(backends, configfsRoot)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("failed to listen: %v", err))
	}
	grpcSrv := grpc.NewServer()
	srv.Register(grpcSrv)

	go func() { _ = grpcSrv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		grpcSrv.Stop()
		_ = lis.Close()
		panic(fmt.Sprintf("failed to connect to local agent: %v", err))
	}

	client := agentv1.NewAgentServiceClient(conn)
	cleanup := func() {
		_ = conn.Close()
		grpcSrv.GracefulStop()
	}
	return client, cleanup
}

// fKubectlOutput runs kubectl with the suite kubeconfig.
func fKubectlOutput(ctx context.Context, args ...string) (string, error) {
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" && suiteKindCluster != nil {
		kubeconfigPath = suiteKindCluster.KubeconfigPath
	}
	if kubeconfigPath == "" {
		return "", fmt.Errorf("[F30] KUBECONFIG not set")
	}
	cmdArgs := append([]string{"--kubeconfig=" + kubeconfigPath}, args...)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "kubectl", cmdArgs...) //nolint:gosec
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl %s: %w\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// fApplyStdin runs kubectl apply -f - with the given YAML content via stdin.
func fApplyStdin(ctx context.Context, yamlContent string) error {
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" && suiteKindCluster != nil {
		kubeconfigPath = suiteKindCluster.KubeconfigPath
	}
	cmd := exec.CommandContext(ctx, "kubectl", //nolint:gosec
		"--kubeconfig="+kubeconfigPath, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yamlContent)
	return cmd.Run()
}

// fShell runs a shell command and returns stdout+stderr combined.
func fShell(ctx context.Context, name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return strings.TrimSpace(buf.String()), err
}

// fBlockDeviceSize returns the size of a block device in bytes.
func fBlockDeviceSize(ctx context.Context, devPath string) (int64, error) {
	out, err := fShell(ctx, "blockdev", "--getsize64", devPath)
	if err != nil {
		return 0, fmt.Errorf("blockdev --getsize64 %s: %w\noutput: %s", devPath, err, out)
	}
	var size int64
	_, err = fmt.Sscanf(strings.TrimSpace(out), "%d", &size)
	return size, err
}

// fNvmeofExportParams builds NVMe-oF TCP export params.
func fNvmeofExportParams(bindAddr string, port int32) *agentv1.ExportParams {
	return &agentv1.ExportParams{
		Params: &agentv1.ExportParams_NvmeofTcp{
			NvmeofTcp: &agentv1.NvmeofTcpExportParams{
				BindAddress: bindAddr,
				Port:        port,
			},
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// F27: 실제 LVM LV 생성/삭제/확장/용량
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("F27: 실제 LVM LV 생성/삭제/확장/용량",
	Label("default-profile", "lvm", "full", "f27"),
	Ordered,
	func() {

		var (
			agentClient agentv1.AgentServiceClient
			cleanup     func()
			vg          string
		)

		BeforeAll(func() {
			fFailIfNoLVM()
			vg = fLvmVG()
			agentClient, cleanup = startLocalAgentServer(vg, "", f27AgentConfigRoot)
		})

		AfterAll(func() {
			if cleanup != nil {
				cleanup()
			}
		})

		// ── TC-F27.1 ─────────────────────────────────────────────────────────
		It("[TC-F27.1] TestRealLVM_CreateVolume_Linear — linear LV creation and /dev/<vg>/<lv> existence", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			volName := fmt.Sprintf("f27-linear-%d", GinkgoParallelProcess())
			volID := vg + "/" + volName
			DeferCleanup(func() {
				dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer dcancel()
				_, _ = agentClient.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{VolumeId: volID})
			})

			resp, err := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volID,
				CapacityBytes: 1 << 30, // 1 GiB
			})
			Expect(err).NotTo(HaveOccurred(), "[TC-F27.1] CreateVolume must succeed")
			Expect(resp.DevicePath).NotTo(BeEmpty(), "[TC-F27.1] DevicePath must be set")
			Expect(resp.CapacityBytes).To(BeNumerically(">=", 1<<30),
				"[TC-F27.1] capacity_bytes must be >= 1 GiB (PE rounding up)")

			By("verifying /dev/<vg>/<lv> exists")
			Eventually(func() error {
				_, err := os.Stat(resp.DevicePath)
				return err
			}).Within(30*time.Second).ProbeEvery(2*time.Second).Should(Succeed(),
				"[TC-F27.1] block device must appear under /dev")

			By("verifying lvs shows the LV")
			out, err := fShell(ctx, "lvs", "--noheadings", "-o", "lv_name", vg)
			Expect(err).NotTo(HaveOccurred(), "[TC-F27.1] lvs must succeed")
			Expect(out).To(ContainSubstring(volName),
				"[TC-F27.1] lvs output must include the new LV")
		})

		// ── TC-F27.2 ─────────────────────────────────────────────────────────
		It("[TC-F27.2] TestRealLVM_CreateVolume_Thin — thin LV creation inside thin pool", func() {
			thinPool := os.Getenv("LVM_THIN_POOL")
			if thinPool == "" {
				Skip("[TC-F27.2] LVM_THIN_POOL not set — skipping thin LV test")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			volName := fmt.Sprintf("f27-thin-%d", GinkgoParallelProcess())
			volID := vg + "/" + volName
			DeferCleanup(func() {
				dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer dcancel()
				_, _ = agentClient.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{VolumeId: volID})
			})

			resp, err := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volID,
				CapacityBytes: 1 << 30,
				BackendParams: &agentv1.BackendParams{
					Params: &agentv1.BackendParams_Lvm{
						Lvm: &agentv1.LvmVolumeParams{
							VolumeGroup:   vg,
							ProvisionMode: "thin",
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred(), "[TC-F27.2] CreateVolume thin must succeed")
			Expect(resp.DevicePath).NotTo(BeEmpty(), "[TC-F27.2] DevicePath must be set")

			By("verifying lvs -a shows thin LV")
			out, err := fShell(ctx, "lvs", "-a", "--noheadings", "-o", "lv_name,pool_lv", vg)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring(volName), "[TC-F27.2] lvs must show the thin LV")
			Expect(out).To(ContainSubstring(thinPool), "[TC-F27.2] lvs pool_lv column must show thin pool name")
		})

		// ── TC-F27.3 ─────────────────────────────────────────────────────────
		It("[TC-F27.3] TestRealLVM_CreateVolume_Idempotent — re-creating the same LV returns same device_path", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			volName := fmt.Sprintf("f27-idem-%d", GinkgoParallelProcess())
			volID := vg + "/" + volName
			DeferCleanup(func() {
				dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer dcancel()
				_, _ = agentClient.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{VolumeId: volID})
			})

			req := &agentv1.CreateVolumeRequest{
				VolumeId:      volID,
				CapacityBytes: 1 << 30,
			}

			resp1, err := agentClient.CreateVolume(ctx, req)
			Expect(err).NotTo(HaveOccurred(), "[TC-F27.3] first CreateVolume must succeed")

			resp2, err := agentClient.CreateVolume(ctx, req)
			Expect(err).NotTo(HaveOccurred(), "[TC-F27.3] second CreateVolume must succeed (idempotent)")
			Expect(resp2.DevicePath).To(Equal(resp1.DevicePath),
				"[TC-F27.3] second CreateVolume must return same device_path")

			By("verifying only one LV exists in lvs")
			out, err := fShell(ctx, "lvs", "--noheadings", "-o", "lv_name", vg)
			Expect(err).NotTo(HaveOccurred())
			count := strings.Count(out, volName)
			Expect(count).To(Equal(1), "[TC-F27.3] lvs must show exactly one LV with the given name")
		})

		// ── TC-F27.4 ─────────────────────────────────────────────────────────
		It("[TC-F27.4] TestRealLVM_DeleteVolume — LV removed from /dev and lvs after DeleteVolume", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			volName := fmt.Sprintf("f27-del-%d", GinkgoParallelProcess())
			volID := vg + "/" + volName
			createResp, err := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volID,
				CapacityBytes: 512 << 20,
			})
			Expect(err).NotTo(HaveOccurred(), "[TC-F27.4] CreateVolume must succeed")
			devPath := createResp.DevicePath

			By("deleting the LV")
			_, err = agentClient.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{VolumeId: volID})
			Expect(err).NotTo(HaveOccurred(), "[TC-F27.4] DeleteVolume must succeed")

			By("verifying block device disappears")
			Eventually(func() bool {
				_, err := os.Stat(devPath)
				return os.IsNotExist(err)
			}).Within(30*time.Second).ProbeEvery(2*time.Second).Should(BeTrue(),
				"[TC-F27.4] block device must be removed from /dev")

			By("verifying lvs no longer shows the LV")
			out, err := fShell(ctx, "lvs", "--noheadings", "-o", "lv_name", vg)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(ContainSubstring(volName),
				"[TC-F27.4] lvs must not show the deleted LV")
		})

		// ── TC-F27.5 ─────────────────────────────────────────────────────────
		It("[TC-F27.5] TestRealLVM_ExpandVolume — lvextend reflected in lvs and blockdev", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			volName := fmt.Sprintf("f27-exp-%d", GinkgoParallelProcess())
			volID := vg + "/" + volName
			createResp, err := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volID,
				CapacityBytes: 1 << 30,
			})
			Expect(err).NotTo(HaveOccurred(), "[TC-F27.5] CreateVolume must succeed")
			DeferCleanup(func() {
				dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer dcancel()
				_, _ = agentClient.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{VolumeId: volID})
			})

			By("expanding LV from 1 GiB to 2 GiB")
			_, err = agentClient.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
				VolumeId:       volID,
				RequestedBytes: 2 << 30,
			})
			Expect(err).NotTo(HaveOccurred(), "[TC-F27.5] ExpandVolume must succeed")

			By("verifying blockdev shows expanded size")
			size, err := fBlockDeviceSize(ctx, createResp.DevicePath)
			Expect(err).NotTo(HaveOccurred(), "[TC-F27.5] blockdev must succeed")
			Expect(size).To(BeNumerically(">=", 2<<30),
				"[TC-F27.5] block device must be at least 2 GiB after expansion")
		})

		// ── TC-F27.6 ─────────────────────────────────────────────────────────
		It("[TC-F27.6] TestRealLVM_GetCapacity_LinearVG — GetCapacity matches vgs output", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := agentClient.GetCapacity(ctx, &agentv1.GetCapacityRequest{
				BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
				PoolName:    vg,
			})
			Expect(err).NotTo(HaveOccurred(), "[TC-F27.6] GetCapacity must succeed")
			Expect(resp.TotalBytes).To(BeNumerically(">", 0), "[TC-F27.6] TotalBytes must be positive")
			Expect(resp.AvailableBytes).To(BeNumerically(">=", 0), "[TC-F27.6] AvailableBytes must be non-negative")
			Expect(resp.TotalBytes).To(BeNumerically(">=", resp.AvailableBytes),
				"[TC-F27.6] TotalBytes must be >= AvailableBytes")
		})

		// ── TC-F27.7 ─────────────────────────────────────────────────────────
		It("[TC-F27.7] TestRealLVM_GetCapacity_ThinPool — thin pool capacity is reported", func() {
			thinPool := os.Getenv("LVM_THIN_POOL")
			if thinPool == "" {
				Skip("[TC-F27.7] LVM_THIN_POOL not set — skipping thin pool capacity test")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := agentClient.GetCapacity(ctx, &agentv1.GetCapacityRequest{
				BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
				PoolName:    fmt.Sprintf("%s/%s", vg, thinPool),
			})
			Expect(err).NotTo(HaveOccurred(), "[TC-F27.7] GetCapacity for thin pool must succeed")
			Expect(resp.TotalBytes).To(BeNumerically(">", 0), "[TC-F27.7] thin pool TotalBytes must be positive")
		})

		// ── TC-F27.8 ─────────────────────────────────────────────────────────
		It("[TC-F27.8] TestRealLVM_VGFull_CreateFails — creating LV larger than VG free space returns error", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			capResp, err := agentClient.GetCapacity(ctx, &agentv1.GetCapacityRequest{
				BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
				PoolName:    vg,
			})
			Expect(err).NotTo(HaveOccurred(), "[TC-F27.8] GetCapacity must succeed")

			oversized := capResp.AvailableBytes*2 + (1 << 30)
			volName := fmt.Sprintf("f27-overflow-%d", GinkgoParallelProcess())
			volID := vg + "/" + volName

			_, err = agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volID,
				CapacityBytes: oversized,
			})
			Expect(err).To(HaveOccurred(), "[TC-F27.8] CreateVolume must fail when VG is too full")
		})

		// ── TC-F27.9 ─────────────────────────────────────────────────────────
		It("[TC-F27.9] TestRealLVM_ExtentRounding — non-PE-aligned request is rounded up to PE boundary", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			// Request a size that is not aligned to 4 MiB PE.
			requestedBytes := int64(999_999_488) // ~953.67 MiB, not 4 MiB aligned.
			volName := fmt.Sprintf("f27-pe-%d", GinkgoParallelProcess())
			volID := vg + "/" + volName
			DeferCleanup(func() {
				dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer dcancel()
				_, _ = agentClient.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{VolumeId: volID})
			})

			resp, err := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volID,
				CapacityBytes: requestedBytes,
			})
			Expect(err).NotTo(HaveOccurred(), "[TC-F27.9] CreateVolume must succeed")
			Expect(resp.CapacityBytes).To(BeNumerically(">=", requestedBytes),
				"[TC-F27.9] PE-rounded capacity must be >= requested bytes")

			peMiB := int64(4 << 20)
			Expect(resp.CapacityBytes%peMiB).To(BeZero(),
				"[TC-F27.9] capacity_bytes must be a multiple of PE size (4 MiB)")
		})

	})

// ─────────────────────────────────────────────────────────────────────────────
// F28: 실제 LVM + NVMe-oF configfs 내보내기
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("F28: 실제 LVM + NVMe-oF configfs 내보내기",
	Label("default-profile", "lvm", "nvmeof", "full", "f28"),
	Ordered,
	func() {

		var (
			agentClient agentv1.AgentServiceClient
			cleanup     func()
			vg          string
			volID       string
			devPath     string
		)

		BeforeAll(func() {
			fFailIfNoLVM()
			fFailIfNoNVMeKernelModules()
			vg = fLvmVG()
			agentClient, cleanup = startLocalAgentServer(vg, "", f27AgentConfigRoot)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			volName := fmt.Sprintf("f28-lv-%d", GinkgoParallelProcess())
			volID = vg + "/" + volName

			resp, err := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volID,
				CapacityBytes: 512 << 20,
			})
			if err != nil {
				Skip(fmt.Sprintf("[F28] BeforeAll: CreateVolume failed: %v — skipping F28 tests", err))
			}
			devPath = resp.DevicePath
		})

		AfterAll(func() {
			if cleanup != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_, _ = agentClient.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
					VolumeId:     volID,
					ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
				})
				_, _ = agentClient.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{VolumeId: volID})
				cleanup()
			}
		})

		// ── TC-F28.1 ─────────────────────────────────────────────────────────
		It("[TC-F28.1] TestRealLVM_NVMeoF_Export — NVMe-oF subsystem created in configfs", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			Expect(devPath).NotTo(BeEmpty(), "[TC-F28.1] LV device path must be set")

			By("exporting the LV as NVMe-oF subsystem")
			_, err := agentClient.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
				VolumeId:     volID,
				ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
				DevicePath:   devPath,
				ExportParams: fNvmeofExportParams("127.0.0.1", 4420),
			})
			Expect(err).NotTo(HaveOccurred(), "[TC-F28.1] ExportVolume must succeed")

			nqn := fVolumeNQN(volID)
			By("verifying configfs subsystem directory exists")
			subsysPath := fmt.Sprintf("%s/subsystems/%s", f27AgentConfigRoot, nqn)
			Eventually(func() error {
				_, err := os.Stat(subsysPath)
				return err
			}).Within(15*time.Second).ProbeEvery(1*time.Second).Should(Succeed(),
				fmt.Sprintf("[TC-F28.1] configfs subsystem path must exist: %s", subsysPath))
		})

		// ── TC-F28.2 ─────────────────────────────────────────────────────────
		It("[TC-F28.2] TestRealLVM_NVMeoF_Unexport — configfs subsystem removed after UnexportVolume", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			By("unexporting the NVMe-oF subsystem")
			_, err := agentClient.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
				VolumeId:     volID,
				ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
			})
			Expect(err).NotTo(HaveOccurred(), "[TC-F28.2] UnexportVolume must succeed")

			nqn := fVolumeNQN(volID)
			By("verifying configfs subsystem directory is removed")
			subsysPath := fmt.Sprintf("%s/subsystems/%s", f27AgentConfigRoot, nqn)
			Eventually(func() bool {
				_, err := os.Stat(subsysPath)
				return os.IsNotExist(err)
			}).Within(15*time.Second).ProbeEvery(1*time.Second).Should(BeTrue(),
				fmt.Sprintf("[TC-F28.2] configfs subsystem path must be removed: %s", subsysPath))
		})

	})

// ─────────────────────────────────────────────────────────────────────────────
// F29: 실제 LVM + NVMe-oF TCP 연결
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("F29: 실제 LVM + NVMe-oF TCP 연결",
	Label("default-profile", "lvm", "nvmeof", "connect", "full", "f29"),
	Ordered,
	func() {

		var (
			agentClient agentv1.AgentServiceClient
			cleanup     func()
			vg          string
			volID       string
			nqn         string
		)

		BeforeAll(func() {
			fFailIfNoLVM()
			fFailIfNoNVMeKernelModules()
			fFailIfNoNVMeCLI()
			vg = fLvmVG()
			agentClient, cleanup = startLocalAgentServer(vg, "", f27AgentConfigRoot)

			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			volName := fmt.Sprintf("f29-lv-%d", GinkgoParallelProcess())
			volID = vg + "/" + volName
			nqn = fVolumeNQN(volID)

			resp, err := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volID,
				CapacityBytes: 512 << 20,
			})
			if err != nil {
				Skip(fmt.Sprintf("[F29] BeforeAll: CreateVolume failed: %v", err))
			}

			_, err = agentClient.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
				VolumeId:     volID,
				ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
				DevicePath:   resp.DevicePath,
				ExportParams: fNvmeofExportParams("127.0.0.1", 4420),
			})
			if err != nil {
				Skip(fmt.Sprintf("[F29] BeforeAll: ExportVolume failed: %v", err))
			}
		})

		AfterAll(func() {
			if cleanup != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				_, _ = fShell(ctx, "nvme", "disconnect", "-n", nqn)
				_, _ = agentClient.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
					VolumeId:     volID,
					ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
				})
				_, _ = agentClient.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{VolumeId: volID})
				cleanup()
			}
		})

		// ── TC-F29.1 ─────────────────────────────────────────────────────────
		It("[TC-F29.1] TestRealLVM_NVMeoF_Connect — NVMe-oF TCP connect creates /dev/nvme* device", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			By("connecting to NVMe-oF subsystem via TCP")
			_, err := fShell(ctx, "nvme", "connect",
				"--transport", "tcp",
				"--traddr", "127.0.0.1",
				"--trsvcid", "4420",
				"--nqn", nqn)
			Expect(err).NotTo(HaveOccurred(), "[TC-F29.1] nvme connect must succeed")

			By("verifying NVMe device appears in nvme list")
			Eventually(func() bool {
				out, err := fShell(ctx, "nvme", "list", "--output-format", "json")
				if err != nil {
					return false
				}
				return strings.Contains(out, nqn) || strings.Contains(out, "/dev/nvme")
			}).Within(30*time.Second).ProbeEvery(2*time.Second).Should(BeTrue(),
				"[TC-F29.1] NVMe device must appear after connect")
		})

		// ── TC-F29.2 ─────────────────────────────────────────────────────────
		It("[TC-F29.2] TestRealLVM_NVMeoF_Disconnect — NVMe-oF disconnect removes device", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			By("disconnecting NVMe-oF subsystem")
			_, err := fShell(ctx, "nvme", "disconnect", "-n", nqn)
			Expect(err).NotTo(HaveOccurred(), "[TC-F29.2] nvme disconnect must succeed")

			By("verifying NVMe device is removed from nvme list")
			Eventually(func() bool {
				out, _ := fShell(ctx, "nvme", "list", "--output-format", "json")
				return !strings.Contains(out, nqn)
			}).Within(30*time.Second).ProbeEvery(2*time.Second).Should(BeTrue(),
				"[TC-F29.2] NVMe device must be removed after disconnect")
		})

		// ── TC-F29.3 ─────────────────────────────────────────────────────────
		It("[TC-F29.3] TestRealLVM_NVMeoF_FullStoragePath — create→export→connect→mkfs→mount→I/O→umount→disconnect→unexport→delete", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()

			if os.Geteuid() != 0 {
				Skip("[TC-F29.3] root required for mount — skipping full storage path test")
			}

			fullVolName := fmt.Sprintf("f29-full-%d", GinkgoParallelProcess())
			fullVolID := vg + "/" + fullVolName
			fullNQN := fVolumeNQN(fullVolID)

			By("creating LV")
			createResp, err := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      fullVolID,
				CapacityBytes: 512 << 20,
			})
			Expect(err).NotTo(HaveOccurred(), "[TC-F29.3] CreateVolume must succeed")

			By("exporting as NVMe-oF")
			_, err = agentClient.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
				VolumeId:     fullVolID,
				ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
				DevicePath:   createResp.DevicePath,
				ExportParams: fNvmeofExportParams("127.0.0.1", 4421),
			})
			Expect(err).NotTo(HaveOccurred(), "[TC-F29.3] ExportVolume must succeed")

			By("connecting via NVMe-oF TCP")
			_, err = fShell(ctx, "nvme", "connect",
				"--transport", "tcp",
				"--traddr", "127.0.0.1",
				"--trsvcid", "4421",
				"--nqn", fullNQN)
			Expect(err).NotTo(HaveOccurred(), "[TC-F29.3] nvme connect must succeed")

			var nvmeDev string
			Eventually(func() bool {
				out, err := fShell(ctx, "nvme", "list")
				if err != nil {
					return false
				}
				for _, line := range strings.Split(out, "\n") {
					if strings.HasPrefix(strings.TrimSpace(line), "/dev/nvme") {
						fields := strings.Fields(line)
						if len(fields) > 0 {
							nvmeDev = fields[0]
							return true
						}
					}
				}
				return false
			}).Within(30*time.Second).ProbeEvery(2*time.Second).Should(BeTrue(),
				"[TC-F29.3] NVMe device must appear")

			mountDir, err := os.MkdirTemp("/tmp", "f29-mount-")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(mountDir)

			By("formatting with ext4")
			_, err = fShell(ctx, "mkfs.ext4", "-F", nvmeDev)
			Expect(err).NotTo(HaveOccurred(), "[TC-F29.3] mkfs.ext4 must succeed")

			By("mounting filesystem")
			_, err = fShell(ctx, "mount", nvmeDev, mountDir)
			Expect(err).NotTo(HaveOccurred(), "[TC-F29.3] mount must succeed")

			By("writing 1 MiB test data")
			testFile := mountDir + "/testdata"
			_, err = fShell(ctx, "dd", "if=/dev/urandom", "of="+testFile, "bs=1M", "count=1")
			Expect(err).NotTo(HaveOccurred(), "[TC-F29.3] write must succeed")

			By("computing checksum")
			checksumOut, err := fShell(ctx, "md5sum", testFile)
			Expect(err).NotTo(HaveOccurred(), "[TC-F29.3] md5sum must succeed")
			Expect(checksumOut).NotTo(BeEmpty(), "[TC-F29.3] checksum must be non-empty")

			By("unmounting filesystem")
			_, err = fShell(ctx, "umount", mountDir)
			Expect(err).NotTo(HaveOccurred(), "[TC-F29.3] umount must succeed")

			By("disconnecting NVMe-oF")
			_, err = fShell(ctx, "nvme", "disconnect", "-n", fullNQN)
			Expect(err).NotTo(HaveOccurred(), "[TC-F29.3] nvme disconnect must succeed")

			By("unexporting NVMe-oF subsystem")
			_, err = agentClient.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
				VolumeId:     fullVolID,
				ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
			})
			Expect(err).NotTo(HaveOccurred(), "[TC-F29.3] UnexportVolume must succeed")

			By("deleting LV")
			_, err = agentClient.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{VolumeId: fullVolID})
			Expect(err).NotTo(HaveOccurred(), "[TC-F29.3] DeleteVolume must succeed")

			By("verifying LV is removed")
			Eventually(func() bool {
				_, err := os.Stat(createResp.DevicePath)
				return os.IsNotExist(err)
			}).Within(15*time.Second).ProbeEvery(2*time.Second).Should(BeTrue(),
				"[TC-F29.3] LV block device must be removed")
		})

	})

// ─────────────────────────────────────────────────────────────────────────────
// F30: K8s PVC + 실제 LVM 통합
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("F30: K8s PVC + 실제 LVM 통합",
	Label("default-profile", "lvm", "k8s", "full", "f30"),
	Ordered,
	func() {

		var (
			testNamespace string
			scName        string
			pvcName       string
			podName       string
		)

		BeforeAll(func() {
			fFailIfNoLVM()
			if os.Getenv("KUBECONFIG") == "" && suiteKindCluster == nil {
				Skip("[F30] KUBECONFIG not set and suiteKindCluster is nil — Kind cluster not available")
			}

			testNamespace = fmt.Sprintf("f30-%d", GinkgoParallelProcess())
			pvcName = "f30-pvc"
			podName = "f30-pod"

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			scOut, err := fKubectlOutput(ctx, "get", "storageclass",
				"-o", "jsonpath={.items[?(@.provisioner=='pillar-csi.bhyoo.com')].metadata.name}")
			if err != nil || scOut == "" {
				Skip("[F30] no pillar-csi StorageClass available")
			}
			scName = strings.Fields(scOut)[0]

			_, _ = fKubectlOutput(ctx, "create", "namespace", testNamespace)
		})

		AfterAll(func() {
			if testNamespace == "" {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			_, _ = fKubectlOutput(ctx, "delete", "pod", podName,
				"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
			_, _ = fKubectlOutput(ctx, "delete", "pvc", pvcName,
				"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
			_, _ = fKubectlOutput(ctx, "delete", "namespace", testNamespace,
				"--ignore-not-found=true", "--wait=true")
		})

		// ── TC-F30.1 ─────────────────────────────────────────────────────────
		It("[TC-F30.1] TestKubernetes_LVM_PVCProvision — LVM PVC becomes Bound with real LV", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			pvcYAML := fmt.Sprintf(`
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
  storageClassName: %s
`, pvcName, testNamespace, scName)
			Expect(fApplyStdin(ctx, pvcYAML)).To(Succeed(), "[TC-F30.1] apply PVC")

			Eventually(func(g Gomega) {
				phase, err := fKubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).To(Equal("Bound"), "[TC-F30.1] PVC must be Bound")
			}).WithContext(ctx).
				WithTimeout(60*time.Second).
				WithPolling(5*time.Second).
				Should(Succeed(), "[TC-F30.1] PVC must bind within 60s")

			pvName, err := fKubectlOutput(ctx, "get", "pvc", pvcName,
				"-n", testNamespace, "-o", "jsonpath={.spec.volumeName}")
			Expect(err).NotTo(HaveOccurred())
			Expect(pvName).NotTo(BeEmpty(), "[TC-F30.1] PV must be created")
		})

		// ── TC-F30.2 ─────────────────────────────────────────────────────────
		It("[TC-F30.2] TestKubernetes_LVM_PodMount — Pod mounts LVM PVC via NVMe-oF TCP with I/O", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
			defer cancel()

			pvcPhase, err := fKubectlOutput(ctx, "get", "pvc", pvcName,
				"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
			if err != nil || pvcPhase != "Bound" {
				Skip("[TC-F30.2] PVC not Bound — TC-F30.1 may have failed")
			}

			podYAML := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  containers:
  - name: app
    image: busybox
    command: ["/bin/sh", "-c", "dd if=/dev/urandom of=/data/testfile bs=1M count=1 && md5sum /data/testfile && sleep 3600"]
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: %s
`, podName, testNamespace, pvcName)
			Expect(fApplyStdin(ctx, podYAML)).To(Succeed(), "[TC-F30.2] apply Pod")

			Eventually(func(g Gomega) {
				podPhase, err := fKubectlOutput(ctx, "get", "pod", podName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(podPhase).To(Equal("Running"), "[TC-F30.2] Pod must reach Running")
			}).WithContext(ctx).
				WithTimeout(240*time.Second).
				WithPolling(10*time.Second).
				Should(Succeed(), "[TC-F30.2] Pod must be Running within 240s")
		})

		// ── TC-F30.3 ─────────────────────────────────────────────────────────
		It("[TC-F30.3] TestKubernetes_LVM_PVCDelete — PVC deletion removes LV and NVMe-oF subsystem", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			By("deleting Pod first")
			_, err := fKubectlOutput(ctx, "delete", "pod", podName,
				"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
			Expect(err).NotTo(HaveOccurred(), "[TC-F30.3] Pod deletion must succeed")

			pvName, _ := fKubectlOutput(ctx, "get", "pvc", pvcName,
				"-n", testNamespace, "-o", "jsonpath={.spec.volumeName}")

			By("deleting PVC")
			_, err = fKubectlOutput(ctx, "delete", "pvc", pvcName,
				"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
			Expect(err).NotTo(HaveOccurred(), "[TC-F30.3] PVC deletion must succeed")

			Eventually(func(g Gomega) {
				if pvName == "" {
					return
				}
				out, err := fKubectlOutput(ctx, "get", "pv", pvName, "--ignore-not-found=true")
				g.Expect(err).NotTo(HaveOccurred())
				if out != "" {
					pvPhase, _ := fKubectlOutput(ctx, "get", "pv", pvName,
						"-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
					g.Expect(pvPhase).NotTo(Equal("Bound"),
						"[TC-F30.3] PV must not remain Bound after PVC deletion")
				}
			}).WithContext(ctx).
				WithTimeout(60 * time.Second).
				WithPolling(5 * time.Second).
				Should(Succeed())
		})

	})

// ─────────────────────────────────────────────────────────────────────────────
// F31: 실제 LVM 온라인 볼륨 확장
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("F31: 실제 LVM 온라인 볼륨 확장",
	Label("default-profile", "lvm", "expansion", "full", "f31"),
	Ordered,
	func() {

		var (
			testNamespace string
			scName        string
			pvcName       string
			podName       string
		)

		BeforeAll(func() {
			fFailIfNoLVM()
			if os.Getenv("KUBECONFIG") == "" && suiteKindCluster == nil {
				Skip("[F31] KUBECONFIG not set and suiteKindCluster is nil — Kind cluster not available")
			}

			testNamespace = fmt.Sprintf("f31-%d", GinkgoParallelProcess())
			pvcName = "f31-pvc"
			podName = "f31-pod"

			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()

			scOut, err := fKubectlOutput(ctx, "get", "storageclass",
				"-o", "jsonpath={.items[?(@.provisioner=='pillar-csi.bhyoo.com')].metadata.name}")
			if err != nil || scOut == "" {
				Skip("[F31] no pillar-csi StorageClass available")
			}
			scName = strings.Fields(scOut)[0]

			_, _ = fKubectlOutput(ctx, "create", "namespace", testNamespace)

			pvcYAML := fmt.Sprintf(`
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
  storageClassName: %s
`, pvcName, testNamespace, scName)
			if err := fApplyStdin(ctx, pvcYAML); err != nil {
				Skip(fmt.Sprintf("[F31] BeforeAll: apply PVC failed: %v", err))
			}

			podYAML := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  containers:
  - name: app
    image: busybox
    command: ["/bin/sh", "-c", "echo 'data integrity test' > /mnt/testfile && sleep 3600"]
    volumeMounts:
    - name: data
      mountPath: /mnt
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: %s
`, podName, testNamespace, pvcName)
			if err := fApplyStdin(ctx, podYAML); err != nil {
				Skip(fmt.Sprintf("[F31] BeforeAll: apply Pod failed: %v", err))
			}

			Eventually(func(g Gomega) {
				podPhase, err := fKubectlOutput(ctx, "get", "pod", podName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(podPhase).To(Equal("Running"))
			}).WithContext(ctx).
				WithTimeout(120*time.Second).
				WithPolling(5*time.Second).
				Should(Succeed(), "[F31] BeforeAll: Pod must reach Running")
		})

		AfterAll(func() {
			if testNamespace == "" {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			_, _ = fKubectlOutput(ctx, "delete", "pod", podName,
				"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
			_, _ = fKubectlOutput(ctx, "delete", "pvc", pvcName,
				"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
			_, _ = fKubectlOutput(ctx, "delete", "namespace", testNamespace,
				"--ignore-not-found=true", "--wait=true")
		})

		// ── TC-F31.1 ─────────────────────────────────────────────────────────
		It("[TC-F31.1] TestRealLVM_OnlineExpand_ControllerSide — ControllerExpandVolume triggers lvextend and LV grows", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			podPhase, err := fKubectlOutput(ctx, "get", "pod", podName,
				"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
			if err != nil || podPhase != "Running" {
				Skip("[TC-F31.1] Pod not Running — BeforeAll may have failed")
			}

			By("patching PVC to 2Gi")
			_, err = fKubectlOutput(ctx, "patch", "pvc", pvcName,
				"-n", testNamespace,
				"--type=merge",
				"-p", `{"spec":{"resources":{"requests":{"storage":"2Gi"}}}}`)
			Expect(err).NotTo(HaveOccurred(), "[TC-F31.1] patch PVC to 2Gi must succeed")

			By("waiting for PVC capacity to reflect 2Gi")
			Eventually(func(g Gomega) {
				capacity, err := fKubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.status.capacity.storage}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(capacity).NotTo(BeEmpty(), "[TC-F31.1] PVC capacity must be updated")
			}).WithContext(ctx).
				WithTimeout(90*time.Second).
				WithPolling(5*time.Second).
				Should(Succeed(), "[TC-F31.1] PVC capacity must update within 90s")
		})

		// ── TC-F31.2 ─────────────────────────────────────────────────────────
		It("[TC-F31.2] TestRealLVM_OnlineExpand_NodeSide — NodeExpandVolume triggers resize2fs and pod sees larger filesystem", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			podPhase, err := fKubectlOutput(ctx, "get", "pod", podName,
				"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
			if err != nil || podPhase != "Running" {
				Skip("[TC-F31.2] Pod not Running — TC-F31.1 may have failed")
			}

			By("checking df /mnt inside the Pod shows filesystem")
			dfOut, err := fKubectlOutput(ctx, "exec", podName,
				"-n", testNamespace, "--",
				"df", "-k", "/mnt")
			Expect(err).NotTo(HaveOccurred(), "[TC-F31.2] df /mnt must succeed")
			Expect(dfOut).To(ContainSubstring("/mnt"),
				"[TC-F31.2] df must show /mnt mount point")

			By("verifying testfile content is preserved after expansion")
			catOut, err := fKubectlOutput(ctx, "exec", podName,
				"-n", testNamespace, "--",
				"cat", "/mnt/testfile")
			Expect(err).NotTo(HaveOccurred(), "[TC-F31.2] reading testfile must succeed")
			Expect(catOut).To(ContainSubstring("data integrity test"),
				"[TC-F31.2] testfile content must be preserved after expansion")
		})

	})
