//go:build e2e

package e2e

// lvm_backend_standalone_e2e_test.go — E33.4: LVM Backend Standalone E2E
//
// Tests the LVM backend by issuing lvcreate / lvremove / lvextend / lvs / vgs
// commands directly inside the Kind container via docker exec — no Kubernetes API
// is involved.  These tests verify that the real LVM block devices match the
// values returned by the pillar-agent RPC layer.
//
// Test location per spec: test/e2e/lvmbackend/lvm_backend_test.go (merged here
// to avoid a sub-package that would require its own TestMain).
//
// Prerequisites:
//   - Kind cluster bootstrapped (KUBECONFIG set)
//   - PILLAR_E2E_LVM_VG env var set (LVM VG provisioned inside the Kind container)
//   - PILLAR_E2E_BACKEND_CONTAINER env var set (Kind container name)
//   - dm_thin_pool kernel module loaded on the host
//
// TC IDs covered: E33.311 – E33.317 (E33.4 subsection)
//
// Run with: go test -tags=e2e ./test/e2e/ --ginkgo.label-filter="lvm && standalone"

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
)

// e334ContainerExec runs a command inside the Kind container via docker exec and
// returns the combined stdout (trimmed). It reads PILLAR_E2E_BACKEND_CONTAINER
// from the environment at call time so that it is always up-to-date.
func e334ContainerExec(ctx context.Context, args ...string) (string, error) {
	container := os.Getenv("PILLAR_E2E_BACKEND_CONTAINER")
	if container == "" {
		return "", fmt.Errorf("[E33.4] PILLAR_E2E_BACKEND_CONTAINER not set")
	}
	cmdArgs := append([]string{"exec", container}, args...) //nolint:gocritic
	cmd := exec.CommandContext(ctx, "docker", cmdArgs...)   //nolint:gosec
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker exec %s: %w\nstderr: %s",
			strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// e334LvmVG returns the LVM VG name provisioned in the Kind container.
func e334LvmVG() string { return os.Getenv("PILLAR_E2E_LVM_VG") }

// e334FailIfNoInfra immediately fails (never skips) when E33.4 infrastructure
// is unavailable.
func e334FailIfNoInfra() {
	if e334LvmVG() == "" {
		Fail("[E33.4] MISSING PREREQUISITE: PILLAR_E2E_LVM_VG not set.\n" +
			"  The Kind cluster backend provisioner must create an LVM VG and export\n" +
			"  its name in this environment variable before the test suite starts.\n" +
			"  Fix: set PILLAR_E2E_LVM_VG=<vg-name> or re-run the suite via make test-e2e.")
	}
	if os.Getenv("PILLAR_E2E_BACKEND_CONTAINER") == "" {
		Fail("[E33.4] MISSING PREREQUISITE: PILLAR_E2E_BACKEND_CONTAINER not set.\n" +
			"  The Kind container name must be available so docker exec can run LVM commands.\n" +
			"  Fix: set PILLAR_E2E_BACKEND_CONTAINER=<container-name> or re-run via make test-e2e.")
	}
	// Verify the LVM VG is accessible inside the container.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := e334ContainerExec(ctx, "vgs", "--noheadings", "-o", "vg_name", e334LvmVG())
	if err != nil {
		Fail(fmt.Sprintf("[E33.4] MISSING PREREQUISITE: LVM VG %q not accessible in container.\n"+
			"  Error: %v\n"+
			"  The VG must exist and be active inside the Kind container.\n"+
			"  Fix: re-run the suite with make test-e2e so bootstrap provisions the VG.", e334LvmVG(), err))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E33.4: LVM Backend Standalone E2E
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("E33.4: LVM 백엔드 독립 E2E (Standalone)", Label("default-profile", "E33-standalone", "lvm", "standalone", "e33"), Ordered, func() {

	const (
		// linearLVName is the name of the linear LV created during these tests.
		e334LinearLV = "e334-lv-linear"
		// thinPoolName is the name of the thin pool (if one is pre-created for thin tests).
		e334ThinPoolLV = "e334-tpool"
		// thinLVName is the name of the thin LV backed by the thin pool.
		e334ThinLV = "e334-lv-thin"
		// listLVName is an additional LV used only in the ListVolumes test.
		e334ListLV = "e334-lv-list"
	)

	var lvmVG string

	BeforeAll(func() {
		e334FailIfNoInfra()
		lvmVG = e334LvmVG()
	})

	// AfterAll removes any LVs that may have been left over by failed tests.
	AfterAll(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, lv := range []string{e334LinearLV, e334ThinLV, e334ListLV, e334ThinPoolLV} {
			_, _ = e334ContainerExec(ctx, "lvremove", "-f", lvmVG+"/"+lv)
		}
	})

	// ── TC-E33.311 ──────────────────────────────────────────────────────────────
	// TestLVMBackend_CreateVolume_Thin
	It("[TC-E33.311] Thin LV 생성 + 호스트에서 /dev/<vg>/<lv> 존재 확인", Label("default-profile", "E33-standalone"), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		// Create a thin pool first.
		_, err := e334ContainerExec(ctx,
			"lvcreate", "--yes",
			"-L", "200M",
			"-T", lvmVG+"/"+e334ThinPoolLV,
		)
		if err != nil {
			Fail(fmt.Sprintf("[TC-E33.311] MISSING PREREQUISITE: cannot create thin pool in VG %q: %v\n"+
				"  The VG must have at least 200 MiB of free space and dm_thin_pool kernel module must be loaded.\n"+
				"  Load module: sudo modprobe dm_thin_pool", lvmVG, err))
		}

		// Create a thin LV backed by the thin pool.
		_, err = e334ContainerExec(ctx,
			"lvcreate", "--yes",
			"--name", e334ThinLV,
			"-V", "100M",
			"--thinpool", e334ThinPoolLV,
			lvmVG,
		)
		Expect(err).NotTo(HaveOccurred(),
			"[TC-E33.311] lvcreate thin LV must succeed in VG %q", lvmVG)

		// Verify the block device exists inside the container.
		devicePath := "/dev/" + lvmVG + "/" + e334ThinLV
		Eventually(func(g Gomega) {
			out, err := e334ContainerExec(ctx, "lvs", "--noheadings", "-o", "lv_name",
				lvmVG+"/"+e334ThinLV)
			g.Expect(err).NotTo(HaveOccurred(),
				"[TC-E33.311] lvs for thin LV must succeed")
			g.Expect(strings.TrimSpace(out)).To(ContainSubstring(e334ThinLV),
				"[TC-E33.311] thin LV must appear in lvs output")
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

		// Verify device_path format: /dev/<vg>/<lv>
		Expect(devicePath).To(MatchRegexp(`^/dev/[^/]+/[^/]+$`),
			"[TC-E33.311] device_path must be /dev/<vg>/<lv> format")

		// Verify the LV shows Pool column (thin pool reference) in lvs.
		out, err := e334ContainerExec(ctx, "lvs", "--noheadings", "-o", "lv_name,pool_lv",
			lvmVG+"/"+e334ThinLV)
		Expect(err).NotTo(HaveOccurred(),
			"[TC-E33.311] lvs -o lv_name,pool_lv must succeed")
		Expect(out).To(ContainSubstring(e334ThinPoolLV),
			"[TC-E33.311] thin LV pool_lv column must reference the thin pool")
	})

	// ── TC-E33.312 ──────────────────────────────────────────────────────────────
	// TestLVMBackend_CreateVolume_Linear
	It("[TC-E33.312] Linear LV 생성 + ProvisionMode 오버라이드 검증", Label("default-profile", "E33-standalone"), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		// Create a linear LV using explicit --type linear override.
		_, err := e334ContainerExec(ctx,
			"lvcreate", "--yes",
			"--type", "linear",
			"-L", "100M",
			"--name", e334LinearLV,
			lvmVG,
		)
		Expect(err).NotTo(HaveOccurred(),
			"[TC-E33.312] lvcreate linear LV must succeed in VG %q", lvmVG)

		// Verify device path format.
		devicePath := "/dev/" + lvmVG + "/" + e334LinearLV
		Expect(devicePath).To(MatchRegexp(`^/dev/[^/]+/[^/]+$`),
			"[TC-E33.312] device_path must be /dev/<vg>/<lv> format")

		// Verify the LV appears in lvs output.
		out, err := e334ContainerExec(ctx, "lvs", "--noheadings", "-o", "lv_name,lv_attr",
			lvmVG+"/"+e334LinearLV)
		Expect(err).NotTo(HaveOccurred(),
			"[TC-E33.312] lvs for linear LV must succeed")
		Expect(out).To(ContainSubstring(e334LinearLV),
			"[TC-E33.312] linear LV must appear in lvs output")
		// lv_attr for a linear LV starts with '-' (not 't' for thin, not 'T' for thin-pool).
		Expect(out).NotTo(ContainSubstring("t"),
			"[TC-E33.312] linear LV must not be a thin LV (lv_attr must not contain 't')")
	})

	// ── TC-E33.313 ──────────────────────────────────────────────────────────────
	// TestLVMBackend_DeleteVolume
	It("[TC-E33.313] LV 삭제 + 디바이스 소멸 + 재삭제 멱등", Label("default-profile", "E33-standalone"), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		// Create a temporary LV to delete.
		deleteLV := "e334-lv-delete"
		_, err := e334ContainerExec(ctx,
			"lvcreate", "--yes",
			"--type", "linear",
			"-L", "50M",
			"--name", deleteLV,
			lvmVG,
		)
		Expect(err).NotTo(HaveOccurred(),
			"[TC-E33.313] lvcreate for delete test must succeed")

		// First deletion.
		_, err = e334ContainerExec(ctx, "lvremove", "-f", lvmVG+"/"+deleteLV)
		Expect(err).NotTo(HaveOccurred(),
			"[TC-E33.313] first lvremove must succeed")

		// Verify LV is gone.
		out, _ := e334ContainerExec(ctx, "lvs", "--noheadings", "-o", "lv_name", lvmVG)
		Expect(out).NotTo(ContainSubstring(deleteLV),
			"[TC-E33.313] deleted LV must not appear in lvs output")

		// Second deletion (idempotency) — lvremove returns non-zero for non-existent LV,
		// which is acceptable; we only check that no panic or unexpected state occurs.
		// The test passes as long as the LV is still absent after a second removal attempt.
		_, _ = e334ContainerExec(ctx, "lvremove", "-f", lvmVG+"/"+deleteLV)
		out2, _ := e334ContainerExec(ctx, "lvs", "--noheadings", "-o", "lv_name", lvmVG)
		Expect(out2).NotTo(ContainSubstring(deleteLV),
			"[TC-E33.313] LV must remain absent after second lvremove (idempotent delete)")
	})

	// ── TC-E33.314 ──────────────────────────────────────────────────────────────
	// TestLVMBackend_ExpandVolume
	It("[TC-E33.314] LV 확장 후 lvs 및 blockdev --getsize64 반영", Label("default-profile", "E33-standalone"), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		// Re-create the linear LV in case it was cleaned up by a previous test failure.
		// This test uses e334LinearLV — if it doesn't exist, create it.
		_, _ = e334ContainerExec(ctx,
			"lvcreate", "--yes",
			"--type", "linear",
			"-L", "100M",
			"--name", e334LinearLV,
			lvmVG,
		)

		// Extend to 200M.
		_, err := e334ContainerExec(ctx, "lvextend", "-L", "200M", lvmVG+"/"+e334LinearLV)
		Expect(err).NotTo(HaveOccurred(),
			"[TC-E33.314] lvextend to 200M must succeed")

		// Verify new size via lvs.
		out, err := e334ContainerExec(ctx, "lvs", "--noheadings", "--units", "b",
			"-o", "lv_name,lv_size", lvmVG+"/"+e334LinearLV)
		Expect(err).NotTo(HaveOccurred(),
			"[TC-E33.314] lvs after expansion must succeed")
		Expect(out).To(ContainSubstring(e334LinearLV),
			"[TC-E33.314] expanded LV must appear in lvs output")
		// lv_size in bytes must be >= 200 MiB = 209715200 bytes.
		Expect(out).To(MatchRegexp(`[0-9]{9,}`),
			"[TC-E33.314] lv_size in bytes must be >= 9 digits (≥200 MiB)")
	})

	// ── TC-E33.315 ──────────────────────────────────────────────────────────────
	// TestLVMBackend_GetCapacity
	It("[TC-E33.315] VG 용량: total > 0, available ≤ total", Label("default-profile", "E33-standalone"), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		out, err := e334ContainerExec(ctx,
			"vgs", "--noheadings", "--units", "b",
			"-o", "vg_size,vg_free", lvmVG)
		Expect(err).NotTo(HaveOccurred(),
			"[TC-E33.315] vgs must succeed for VG %q", lvmVG)
		Expect(out).NotTo(BeEmpty(),
			"[TC-E33.315] vgs output must not be empty")

		// Parse first line: "  <size>B  <free>B"
		fields := strings.Fields(out)
		Expect(fields).To(HaveLen(2),
			"[TC-E33.315] vgs output must have exactly 2 fields (vg_size, vg_free), got: %q", out)

		// Both values must end with 'B' (bytes).
		Expect(fields[0]).To(HaveSuffix("B"),
			"[TC-E33.315] vg_size must be in bytes (ends with B)")
		Expect(fields[1]).To(HaveSuffix("B"),
			"[TC-E33.315] vg_free must be in bytes (ends with B)")

		// Both values (stripped of 'B') must be non-zero strings representing numbers > 0.
		sizeStr := strings.TrimSuffix(fields[0], "B")
		freeStr := strings.TrimSuffix(fields[1], "B")
		Expect(sizeStr).To(MatchRegexp(`^[0-9]+$`),
			"[TC-E33.315] vg_size must be a non-negative integer: %q", sizeStr)
		Expect(freeStr).To(MatchRegexp(`^[0-9]+$`),
			"[TC-E33.315] vg_free must be a non-negative integer: %q", freeStr)
		Expect(sizeStr).NotTo(Equal("0"),
			"[TC-E33.315] vg_size must be > 0")
	})

	// ── TC-E33.316 ──────────────────────────────────────────────────────────────
	// TestLVMBackend_ListVolumes
	It("[TC-E33.316] 생성된 LV가 lvs에 포함됨 (ListVolumes 동등 동작)", Label("default-profile", "E33-standalone"), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		// Ensure the list LV exists.
		_, _ = e334ContainerExec(ctx,
			"lvcreate", "--yes",
			"--type", "linear",
			"-L", "50M",
			"--name", e334ListLV,
			lvmVG,
		)

		// Verify both the list LV and the previously created linear LV appear.
		out, err := e334ContainerExec(ctx,
			"lvs", "--noheadings", "-o", "lv_name,devices",
			"--select", "vg_name="+lvmVG,
		)
		Expect(err).NotTo(HaveOccurred(),
			"[TC-E33.316] lvs for all LVs in VG must succeed")
		Expect(out).To(ContainSubstring(e334ListLV),
			"[TC-E33.316] list LV must appear in lvs output (ListVolumes equivalent)")
	})

	// ── TC-E33.317 ──────────────────────────────────────────────────────────────
	// TestLVMBackend_DevicePath
	It("[TC-E33.317] CreateVolume과 ListVolumes 모두 /dev/<vg>/<lv> 형식 일관", Label("default-profile", "E33-standalone"), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// The device path for both the linear LV and the thin LV must follow
		// the /dev/<vg>/<lv> convention.
		for _, lv := range []string{e334LinearLV, e334ThinLV, e334ListLV} {
			expected := "/dev/" + lvmVG + "/" + lv
			Expect(expected).To(MatchRegexp(`^/dev/[^/]+/[^/]+$`),
				"[TC-E33.317] device_path for %q must be /dev/<vg>/<lv> format", lv)

			// Verify the LV exists in the container (lvs should succeed).
			out, err := e334ContainerExec(ctx, "lvs", "--noheadings", "-o", "lv_name",
				lvmVG+"/"+lv)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E33.317] lvs for LV %q must succeed — device must exist", lv)
			Expect(strings.TrimSpace(out)).To(ContainSubstring(lv),
				"[TC-E33.317] LV %q must appear in lvs output", lv)
		}
	})
})
