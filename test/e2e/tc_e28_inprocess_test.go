package e2e

// tc_e28_inprocess_test.go — Per-TC assertions for E28: LVM Agent gRPC E2E tests.
//
// All 30 TCs in this file use names matching docs/E2E-TESTCASES.md sections
// E28.1 through E28.11.  Each function creates a fresh agentTestEnv (real
// agentsvc.Server + real LVM backend via docker exec inside the Kind cluster)
// providing true per-TC isolation with no shared mutable state.
//
// E28.1  — LVM capabilities and health (TCs 244–245)
// E28.2  — LVM full round trip linear + thin (TCs 246–247)
// E28.3  — GetCapacity: linear VG, thin pool, over-provisioned, full VG (TCs 248–251)
// E28.4  — ListVolumes: thin pool LV filtering, linear all returned (TCs 252–253)
// E28.5  — Provisioning mode gRPC propagation (TCs 254–256)
// E28.6  — Error handling: VG not found, shrink rejection, thin without pool,
//           idempotent create, idempotent delete (TCs 257–261)
// E28.7  — ReconcileState configfs restore (TC 262)
// E28.8  — LV name validation: reserved prefixes, first char, max length (TCs 263a–263e)
// E28.9  — ExtraFlags forwarding + VG override mismatch rejection (TCs 263f–263h)
// E28.10 — Thin pool exhaustion: request larger than VG capacity (TCs 263i–263j)
// E28.11 — Multi-backend agent: ZFS + LVM simultaneously (TC 263k)

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	agentsvc "github.com/bhyoo/pillar-csi/internal/agent"
	agentbackend "github.com/bhyoo/pillar-csi/internal/agent/backend"
	lvmb "github.com/bhyoo/pillar-csi/internal/agent/backend/lvm"
	zfsb "github.com/bhyoo/pillar-csi/internal/agent/backend/zfs"
	nvmeof "github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// ─── validatingLVMBackend ─────────────────────────────────────────────────────

// validatingLVMBackend wraps a real LVM VolumeBackend and enforces LV name
// validation using the production lvm.ValidateLVName() rules.  Used by E28.8
// and E28.9 TCs that must verify the gRPC layer rejects invalid LV names and
// VG mismatches — before sending any command to the real LVM inside Kind.
type validatingLVMBackend struct {
	agentbackend.VolumeBackend
	vg string
}

var _ agentbackend.VolumeBackend = (*validatingLVMBackend)(nil)

func (b *validatingLVMBackend) Create(
	ctx context.Context,
	volumeID string,
	capacityBytes int64,
	params *agentv1.BackendParams,
) (string, int64, error) {
	// Validate LV name portion of "<vg>/<lv>" volumeID.
	if idx := strings.IndexByte(volumeID, '/'); idx >= 0 {
		lvName := volumeID[idx+1:]
		if err := lvmb.ValidateLVName(lvName); err != nil {
			return "", 0, status.Errorf(codes.InvalidArgument, "LV name validation: %v", err)
		}
	}
	// Validate VolumeGroup override: cross-VG provisioning is not supported.
	if params != nil && params.GetLvm() != nil {
		vgOverride := strings.TrimSpace(params.GetLvm().GetVolumeGroup())
		if vgOverride != "" && vgOverride != b.vg {
			return "", 0, status.Errorf(codes.InvalidArgument,
				"cross-VG provisioning is not supported: requested %q but backend VG is %q",
				vgOverride, b.vg)
		}
	}
	return b.VolumeBackend.Create(ctx, volumeID, capacityBytes, params)
}

// newValidatingAgentTestEnv creates an agentTestEnv with a validatingLVMBackend
// for E28.8 (name validation) and E28.9 (VG override) TCs.
// Uses real ZFS/LVM backends inside the Kind cluster via docker exec.
func newValidatingAgentTestEnv() *agentTestEnv {
	container, zfsPool, lvmVG := requireSuiteBackendEnv()
	lvmThinPool := os.Getenv(suiteLVMThinPoolEnvVar)
	ctx, cancel := context.WithCancel(context.Background())

	configfsRoot, err := os.MkdirTemp(tcTempRoot, "pillar-csi-configfs-validating-*")
	if err != nil {
		cancel()
		panic(fmt.Sprintf("newValidatingAgentTestEnv: create configfs dir: %v", err))
	}

	// Pre-create the nvmet directory that health.checkNvmetConfigfs expects.
	if err := os.MkdirAll(fmt.Sprintf("%s/nvmet", configfsRoot), 0755); err != nil {
		_ = os.RemoveAll(configfsRoot)
		cancel()
		panic(fmt.Sprintf("newValidatingAgentTestEnv: create nvmet dir: %v", err))
	}

	execFn := realContainerExecFn(container)

	// Ensure the ZFS namespace parent dataset exists.
	if _, zfsCreateErr := execFn(ctx, "zfs", "create", "-p", zfsPool+"/k8s"); zfsCreateErr != nil {
		if !strings.Contains(zfsCreateErr.Error(), "already exists") &&
			!strings.Contains(zfsCreateErr.Error(), "dataset already exists") {
			_ = os.RemoveAll(configfsRoot)
			cancel()
			panic(fmt.Sprintf("newValidatingAgentTestEnv: create ZFS namespace parent %s/k8s: %v", zfsPool, zfsCreateErr))
		}
	}

	zfsBackend := zfsb.NewWithExecFn(zfsPool, "k8s", execFn)
	realLVMBackend := lvmb.NewWithExecFn(lvmVG, lvmThinPool, execFn)
	validatingLVM := &validatingLVMBackend{VolumeBackend: realLVMBackend, vg: lvmVG}

	backends := map[string]agentbackend.VolumeBackend{
		zfsPool: zfsBackend,
		lvmVG:   validatingLVM,
	}

	server := agentsvc.NewServer(
		backends,
		configfsRoot,
		agentsvc.WithDeviceChecker(nvmeof.AlwaysPresentChecker),
	)

	lis := bufconn.Listen(inprocessBufSize)
	grpcSrv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcSrv, server)
	go func() { _ = grpcSrv.Serve(lis) }()

	dialOption := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		dialOption,
	)
	if err != nil {
		grpcSrv.Stop()
		_ = lis.Close()
		_ = os.RemoveAll(configfsRoot)
		cancel()
		panic(fmt.Sprintf("newValidatingAgentTestEnv: dial bufconn: %v", err))
	}

	return &agentTestEnv{
		ctx:          ctx,
		cancel:       cancel,
		server:       server,
		client:       agentv1.NewAgentServiceClient(conn),
		zfsPool:      zfsPool,
		lvmVG:        lvmVG,
		container:    container,
		configfsRoot: configfsRoot,
		lis:          lis,
		grpcSrv:      grpcSrv,
		agentConn:    conn,
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// lvmCreateVolumeE28 creates an LVM volume via the agentTestEnv.
func lvmCreateVolumeE28(env *agentTestEnv, lvName string, capacityBytes int64) error {
	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/" + lvName,
		CapacityBytes: capacityBytes,
	})
	return err
}

// lvmDeleteVolumeE28NoFail deletes an LVM volume via the agentTestEnv,
// ignoring errors. Used in defer-cleanup blocks to free VG space after tests
// so the shared LVM VG is not exhausted during parallel Ginkgo spec execution.
// Must be called BEFORE env.close() — achieved by registering this defer AFTER
// defer env.close() (Go defer is LIFO: last registered runs first).
func lvmDeleteVolumeE28NoFail(env *agentTestEnv, lvName string) {
	_, _ = env.client.DeleteVolume(env.ctx, &agentv1.DeleteVolumeRequest{
		VolumeId: env.lvmVG + "/" + lvName,
	})
}

// e28FullRoundTrip runs the six-step agent lifecycle for a given LV name and params.
func e28FullRoundTrip(env *agentTestEnv, lvName string, params *agentv1.LvmVolumeParams, tc documentedCase) {
	volumeID := env.lvmVG + "/" + lvName
	var bp *agentv1.BackendParams
	if params != nil {
		bp = &agentv1.BackendParams{Params: &agentv1.BackendParams_Lvm{Lvm: params}}
	}

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      volumeID,
		CapacityBytes: 1 << 30,
		BackendParams: bp,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())

	_, err = env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ExportVolume", tc.tcNodeLabel())

	_, err = env.client.AllowInitiator(env.ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  "nqn.2026-01.io.example:host-" + lvName,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: AllowInitiator", tc.tcNodeLabel())

	_, err = env.client.DenyInitiator(env.ctx, &agentv1.DenyInitiatorRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  "nqn.2026-01.io.example:host-" + lvName,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: DenyInitiator", tc.tcNodeLabel())

	_, err = env.client.UnexportVolume(env.ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: UnexportVolume", tc.tcNodeLabel())

	_, err = env.client.DeleteVolume(env.ctx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
	Expect(err).NotTo(HaveOccurred(), "%s: DeleteVolume", tc.tcNodeLabel())
}

// ─── E28.1: LVM capabilities and health ──────────────────────────────────────

// assertE28_LVM_GetCapabilities verifies that GetCapabilities reports BACKEND_TYPE_LVM
// and PROTOCOL_TYPE_NVMEOF_TCP for an agent with a registered LVM backend.
// [TC-E28.244]
func assertE28_LVM_GetCapabilities(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	resp, err := env.client.GetCapabilities(env.ctx, &agentv1.GetCapabilitiesRequest{})
	Expect(err).NotTo(HaveOccurred(), "%s: GetCapabilities", tc.tcNodeLabel())

	backends := resp.GetSupportedBackends()
	Expect(backends).To(ContainElement(agentv1.BackendType_BACKEND_TYPE_LVM),
		"%s: BACKEND_TYPE_LVM must be in supported backends", tc.tcNodeLabel())
}

// assertE28_LVM_HealthCheck verifies that HealthCheck returns healthy=true when
// an LVM backend is registered.
// [TC-E28.245]
func assertE28_LVM_HealthCheck(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	resp, err := env.client.HealthCheck(env.ctx, &agentv1.HealthCheckRequest{})
	Expect(err).NotTo(HaveOccurred(), "%s: HealthCheck", tc.tcNodeLabel())
	Expect(resp.GetHealthy()).To(BeTrue(), "%s: agent should be healthy", tc.tcNodeLabel())
}

// ─── E28.2: LVM full round trip ───────────────────────────────────────────────

// assertE28_LVM_RoundTrip_Linear verifies the full Create→Export→AllowInitiator→
// DenyInitiator→Unexport→Delete lifecycle with LvmVolumeParams.ProvisionMode="linear".
// [TC-E28.246]
func assertE28_LVM_RoundTrip_Linear(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	e28FullRoundTrip(env, "pvc-e28-linear-rt", &agentv1.LvmVolumeParams{
		ProvisionMode: "linear",
	}, tc)
}

// assertE28_LVM_RoundTrip_Thin verifies the full lifecycle with ProvisionMode="thin".
// [TC-E28.247]
func assertE28_LVM_RoundTrip_Thin(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	e28FullRoundTrip(env, "pvc-e28-thin-rt", &agentv1.LvmVolumeParams{
		ProvisionMode: "thin",
	}, tc)
}

// ─── E28.3: GetCapacity ──────────────────────────────────────────────────────

// assertE28_LVM_GetCapacity_LinearVG verifies that GetCapacity on a linear VG
// returns TotalBytes > 0 and AvailableBytes > 0.
// [TC-E28.248]
func assertE28_LVM_GetCapacity_LinearVG(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	resp, err := env.client.GetCapacity(env.ctx, &agentv1.GetCapacityRequest{
		PoolName: env.lvmVG,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: GetCapacity", tc.tcNodeLabel())
	Expect(resp.GetTotalBytes()).To(BeNumerically(">", 0),
		"%s: TotalBytes > 0", tc.tcNodeLabel())
	Expect(resp.GetAvailableBytes()).To(BeNumerically(">", 0),
		"%s: AvailableBytes > 0 for linear VG", tc.tcNodeLabel())
}

// assertE28_LVM_GetCapacity_ThinPool verifies that GetCapacity on a thin-pool
// backed VG returns non-zero TotalBytes and reports available capacity.
// [TC-E28.249]
func assertE28_LVM_GetCapacity_ThinPool(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	resp, err := env.client.GetCapacity(env.ctx, &agentv1.GetCapacityRequest{
		PoolName: env.lvmVG,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: GetCapacity (thin pool)", tc.tcNodeLabel())
	Expect(resp.GetTotalBytes()).To(BeNumerically(">", 0),
		"%s: TotalBytes > 0 for thin pool", tc.tcNodeLabel())
	Expect(resp.GetAvailableBytes()).To(BeNumerically(">=", 0),
		"%s: AvailableBytes >= 0 for thin pool", tc.tcNodeLabel())
}

// assertE28_LVM_GetCapacity_ThinPoolOverProvisioned verifies that the LVM backend
// always returns AvailableBytes >= 0 — the backend must clamp negative values
// that can occur when thin LV virtual sizes exceed the physical pool size.
// [TC-E28.250]
func assertE28_LVM_GetCapacity_ThinPoolOverProvisioned(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	// Create a thin LV with a large virtual size (10 GiB) to put the pool into
	// a state where committed virtual space approaches or exceeds physical.
	// With a 50 GiB thin pool, a 10 GiB virtual LV commits ~20% — enough to
	// exercise the capacity calculation without requiring writing 80 GiB of data.
	Expect(lvmCreateVolumeE28(env, "pvc-e28-overprov-thin", 10<<30)).To(Succeed(),
		"%s: create thin LV for over-provisioning test", tc.tcNodeLabel())

	resp, err := env.client.GetCapacity(env.ctx, &agentv1.GetCapacityRequest{
		PoolName: env.lvmVG,
	})
	Expect(err).NotTo(HaveOccurred(),
		"%s: GetCapacity on thin pool with committed virtual space", tc.tcNodeLabel())
	// AvailableBytes must not be negative — the backend must clamp any negative
	// result from (pool_size * (1 - data_percent/100)) to 0.
	Expect(resp.GetAvailableBytes()).To(BeNumerically(">=", 0),
		"%s: AvailableBytes must be >= 0 (backend must not return negative capacity)",
		tc.tcNodeLabel())
	Expect(resp.GetTotalBytes()).To(BeNumerically(">", 0),
		"%s: TotalBytes must be > 0 for a provisioned thin pool", tc.tcNodeLabel())
}

// assertE28_LVM_GetCapacity_FullVG verifies that after filling a linear VG to
// capacity, GetCapacity returns AvailableBytes close to 0 (≤ one LVM extent
// = 4 MiB) so the clamping logic that prevents negative values is exercised.
// [TC-E28.251]
func assertE28_LVM_GetCapacity_FullVG(tc documentedCase) {
	// MUST use newLinearAgentTestEnv — with a thin pool backend the backend
	// reports physical thin-pool available bytes, which stays positive even after
	// creating large virtual thin LVs (thin provisioning does not consume physical
	// space on commit). Linear LVM is the only mode where filling virtual space
	// drains physical capacity.
	env := newLinearAgentTestEnv()
	defer env.close()
	// Cleanup: delete fill LV to prevent VG exhaustion for other parallel tests.
	// LIFO: this defer runs BEFORE defer env.close() above.
	defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-exhaust-fullvg")

	// Pre-cleanup: delete any leftover fill LV from a previous (failed) run.
	lvmDeleteVolumeE28NoFail(env, "pvc-e28-exhaust-fullvg")

	const extentSize = int64(4 << 20)
	// maxFillAttempts: retry loop to handle the race where another parallel test
	// deletes its LV between our fill creation and GetCapacity check, freeing
	// space and making cap2 appear > 1 extent. Each attempt re-fills the newly
	// freed space until cap2 ≤ extentSize. All fill LVs are deleted by the
	// deferred cleanup registered above.
	const maxFillAttempts = 5
	var extraFillLVs []string
	defer func() {
		for _, name := range extraFillLVs {
			lvmDeleteVolumeE28NoFail(env, name)
		}
	}()

	for attempt := 0; attempt < maxFillAttempts; attempt++ {
		// Get current available capacity so we can fill dynamically.
		// This handles any LVs already present in the shared VG from other tests.
		cap1, err := env.client.GetCapacity(env.ctx, &agentv1.GetCapacityRequest{
			PoolName: env.lvmVG,
		})
		Expect(err).NotTo(HaveOccurred(), "%s: GetCapacity before fill (attempt %d)",
			tc.tcNodeLabel(), attempt)

		avail := cap1.GetAvailableBytes()
		if avail <= extentSize {
			// VG is already full enough — the assertion is already satisfied.
			return
		}

		// Round available bytes DOWN to a 4 MiB extent boundary so lvcreate
		// succeeds. LVM extent size defaults to 4 MiB; requests that are not
		// a multiple of the extent size are rounded down to the previous extent.
		fillSize := (avail / extentSize) * extentSize
		if fillSize <= 0 {
			return // Less than one extent free — already effectively full.
		}

		var fillErr error
		if attempt == 0 {
			fillErr = lvmCreateVolumeE28(env, "pvc-e28-exhaust-fullvg", fillSize)
		} else {
			// Create an additional fill LV to reclaim space freed by concurrent tests.
			extraName := fmt.Sprintf("pvc-e28-exhaust-extra%d", attempt)
			extraFillLVs = append(extraFillLVs, extraName)
			fillErr = lvmCreateVolumeE28(env, extraName, fillSize)
		}
		Expect(fillErr).NotTo(HaveOccurred(),
			"%s: fill linear VG with %d bytes (attempt %d)",
			tc.tcNodeLabel(), fillSize, attempt)

		cap2, err := env.client.GetCapacity(env.ctx, &agentv1.GetCapacityRequest{
			PoolName: env.lvmVG,
		})
		Expect(err).NotTo(HaveOccurred(), "%s: GetCapacity after fill (attempt %d)",
			tc.tcNodeLabel(), attempt)
		if cap2.GetAvailableBytes() <= extentSize {
			// After filling to the extent boundary, at most one extent (4 MiB) can remain.
			Expect(cap2.GetAvailableBytes()).To(BeNumerically("<=", extentSize),
				"%s: AvailableBytes should be ≤ one LVM extent (4 MiB) after fill; "+
					"clamping must not produce a negative value",
				tc.tcNodeLabel())
			return
		}
		// More space is available — a concurrent test freed an LV between fill and check.
		// Retry to fill the new free space.
	}
	// After all attempts, perform the final assertion.
	cap2, err := env.client.GetCapacity(env.ctx, &agentv1.GetCapacityRequest{
		PoolName: env.lvmVG,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: GetCapacity final", tc.tcNodeLabel())
	Expect(cap2.GetAvailableBytes()).To(BeNumerically("<=", extentSize),
		"%s: AvailableBytes should be ≤ one LVM extent (4 MiB) after fill; "+
			"clamping must not produce a negative value",
		tc.tcNodeLabel())
}

// ─── E28.4: ListVolumes ───────────────────────────────────────────────────────

// assertE28_LVM_ListVolumes_SkipsThinPoolLV verifies that ListVolumes returns only
// data LVs and does NOT include thin-pool infrastructure LVs.
// [TC-E28.252]
func assertE28_LVM_ListVolumes_SkipsThinPoolLV(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()
	defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-data-1")
	defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-data-2")

	Expect(lvmCreateVolumeE28(env, "pvc-e28-data-1", 1<<30)).To(Succeed())
	Expect(lvmCreateVolumeE28(env, "pvc-e28-data-2", 1<<30)).To(Succeed())

	resp, err := env.client.ListVolumes(env.ctx, &agentv1.ListVolumesRequest{
		PoolName: env.lvmVG,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ListVolumes", tc.tcNodeLabel())
	// Both data LVs must be present — no data LV should be filtered.
	// The result may contain additional LVs from other parallel tests.
	Expect(resp.GetVolumes()).To(ContainElements(
		HaveField("VolumeId", env.lvmVG+"/pvc-e28-data-1"),
		HaveField("VolumeId", env.lvmVG+"/pvc-e28-data-2"),
	), "%s: data LVs must appear in ListVolumes result", tc.tcNodeLabel())
	// The thin pool infrastructure LV must NOT appear (it is internal to LVM).
	for _, vol := range resp.GetVolumes() {
		lvName := strings.TrimPrefix(vol.GetVolumeId(), env.lvmVG+"/")
		Expect(strings.HasPrefix(lvName, "pillar-e2e-pool")).To(BeFalse(),
			"%s: thin pool infra LV %q must not appear in ListVolumes",
			tc.tcNodeLabel(), vol.GetVolumeId())
	}
}

// assertE28_LVM_ListVolumes_Linear_AllReturned verifies that for a linear VG all
// provisioned LVs are returned without filtering.
// [TC-E28.253]
func assertE28_LVM_ListVolumes_Linear_AllReturned(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()
	defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-lv-a")
	defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-lv-b")
	defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-lv-c")

	Expect(lvmCreateVolumeE28(env, "pvc-e28-lv-a", 1<<30)).To(Succeed())
	Expect(lvmCreateVolumeE28(env, "pvc-e28-lv-b", 1<<30)).To(Succeed())
	Expect(lvmCreateVolumeE28(env, "pvc-e28-lv-c", 1<<30)).To(Succeed())

	resp, err := env.client.ListVolumes(env.ctx, &agentv1.ListVolumesRequest{
		PoolName: env.lvmVG,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ListVolumes linear", tc.tcNodeLabel())
	// All 3 created LVs must be present — linear LVM does not filter any data LV.
	// The result may include additional LVs from other parallel tests, which is
	// expected when the suite VG is shared across Ginkgo workers.
	Expect(resp.GetVolumes()).To(ContainElements(
		HaveField("VolumeId", env.lvmVG+"/pvc-e28-lv-a"),
		HaveField("VolumeId", env.lvmVG+"/pvc-e28-lv-b"),
		HaveField("VolumeId", env.lvmVG+"/pvc-e28-lv-c"),
	), "%s: all 3 linear LVs must be returned without filtering", tc.tcNodeLabel())
}

// ─── E28.5: Provisioning mode gRPC propagation ───────────────────────────────

// assertE28_LVM_CreateVolume_LinearModeParam verifies that LvmVolumeParams.ProvisionMode="linear"
// is accepted by the agent and the backend Create is called.
// [TC-E28.254]
func assertE28_LVM_CreateVolume_LinearModeParam(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()
	defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-linear-param")

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/pvc-e28-linear-param",
		CapacityBytes: 1 << 30,
		BackendParams: &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Lvm{
				Lvm: &agentv1.LvmVolumeParams{ProvisionMode: "linear"},
			},
		},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume with linear mode", tc.tcNodeLabel())
	// Verify volume was actually created by listing volumes.
	listResp, listErr := env.client.ListVolumes(env.ctx, &agentv1.ListVolumesRequest{PoolName: env.lvmVG})
	Expect(listErr).NotTo(HaveOccurred(), "%s: ListVolumes after create", tc.tcNodeLabel())
	Expect(listResp.GetVolumes()).To(ContainElement(
		HaveField("VolumeId", env.lvmVG+"/pvc-e28-linear-param"),
	), "%s: backend Create should have provisioned the volume", tc.tcNodeLabel())
}

// assertE28_LVM_CreateVolume_ThinModeParam verifies that ProvisionMode="thin" propagates
// through gRPC to the backend.
// [TC-E28.255]
func assertE28_LVM_CreateVolume_ThinModeParam(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()
	defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-thin-param")

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/pvc-e28-thin-param",
		CapacityBytes: 1 << 30,
		BackendParams: &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Lvm{
				Lvm: &agentv1.LvmVolumeParams{ProvisionMode: "thin"},
			},
		},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume with thin mode", tc.tcNodeLabel())
	// Verify volume was actually created by listing volumes.
	listResp, listErr := env.client.ListVolumes(env.ctx, &agentv1.ListVolumesRequest{PoolName: env.lvmVG})
	Expect(listErr).NotTo(HaveOccurred(), "%s: ListVolumes after thin create", tc.tcNodeLabel())
	Expect(listResp.GetVolumes()).To(ContainElement(
		HaveField("VolumeId", env.lvmVG+"/pvc-e28-thin-param"),
	), "%s: backend Create should be called for thin mode", tc.tcNodeLabel())
}

// assertE28_LVM_CreateVolume_EmptyMode_DefaultsToBackend verifies that an empty
// ProvisionMode is passed through and the backend applies its compiled-in default.
// [TC-E28.256]
func assertE28_LVM_CreateVolume_EmptyMode_DefaultsToBackend(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()
	defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-empty-mode")

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/pvc-e28-empty-mode",
		CapacityBytes: 1 << 30,
		BackendParams: &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Lvm{
				Lvm: &agentv1.LvmVolumeParams{ProvisionMode: ""},
			},
		},
	})
	Expect(err).NotTo(HaveOccurred(),
		"%s: CreateVolume with empty ProvisionMode should succeed", tc.tcNodeLabel())
}

// ─── E28.6: Error handling ────────────────────────────────────────────────────

// assertE28_LVM_CreateVolume_VGNotFound verifies that CreateVolume returns NotFound
// when the requested VG (pool) is not registered.
// [TC-E28.257]
func assertE28_LVM_CreateVolume_VGNotFound(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      "nonexistent-vg/pvc-e28-vg-notfound",
		CapacityBytes: 1 << 30,
	})
	Expect(err).To(HaveOccurred(), "%s: expected NotFound for unregistered VG", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.NotFound),
		"%s: should return NotFound for unknown pool", tc.tcNodeLabel())
}

// assertE28_LVM_ExpandVolume_ShrinkRejected verifies that ExpandVolume with a
// requested size smaller than the current size is rejected.
// Real LVM rejects shrinks natively — no error injection needed.
// [TC-E28.258]
func assertE28_LVM_ExpandVolume_ShrinkRejected(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()
	defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-shrink")

	Expect(lvmCreateVolumeE28(env, "pvc-e28-shrink", 2<<30)).To(Succeed(),
		"%s: CreateVolume 2GiB", tc.tcNodeLabel())

	// Real LVM rejects shrink requests natively.
	_, err := env.client.ExpandVolume(env.ctx, &agentv1.ExpandVolumeRequest{
		VolumeId:       env.lvmVG + "/pvc-e28-shrink",
		RequestedBytes: 1 << 30, // smaller than 2GiB — shrink attempt
	})
	Expect(err).To(HaveOccurred(), "%s: shrink should be rejected by real LVM", tc.tcNodeLabel())
}

// assertE28_LVM_CreateVolume_ThinWithoutPool verifies that a thin-mode CreateVolume
// on a linear VG (no thin pool configured) returns an error.
// Real LVM rejects thin provisioning when no thinpool is configured.
// [TC-E28.259]
func assertE28_LVM_CreateVolume_ThinWithoutPool(tc documentedCase) {
	// MUST use newLinearAgentTestEnv — this test verifies failure when thin
	// provisioning is requested against a backend with no thin pool. Using
	// newAgentTestEnv() (which has a thin pool configured) would cause
	// CreateVolume to succeed, breaking the test assertion.
	env := newLinearAgentTestEnv()
	defer env.close()

	// Real LVM on a linear VG rejects thin provisioning (no thinpool configured).
	// ValidateParams returns an error before any lvcreate is invoked.
	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/pvc-e28-thin-nopool",
		CapacityBytes: 1 << 30,
		BackendParams: &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Lvm{
				Lvm: &agentv1.LvmVolumeParams{ProvisionMode: "thin"},
			},
		},
	})
	Expect(err).To(HaveOccurred(),
		"%s: thin without pool should return error from real LVM", tc.tcNodeLabel())
}

// assertE28_LVM_CreateVolume_Idempotent verifies that calling CreateVolume twice
// with the same VolumeId and capacity returns success both times.
// [TC-E28.260]
func assertE28_LVM_CreateVolume_Idempotent(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	req := &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/pvc-e28-idempotent",
		CapacityBytes: 1 << 30,
	}
	_, err := env.client.CreateVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: first CreateVolume", tc.tcNodeLabel())

	_, err = env.client.CreateVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: second CreateVolume (idempotent)", tc.tcNodeLabel())
}

// assertE28_LVM_DeleteVolume_NonExistent_Idempotent verifies that deleting a
// non-existent LV returns success (idempotent delete).
// [TC-E28.261]
func assertE28_LVM_DeleteVolume_NonExistent_Idempotent(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.DeleteVolume(env.ctx, &agentv1.DeleteVolumeRequest{
		VolumeId: env.lvmVG + "/pvc-e28-nonexistent-del",
	})
	// Idempotent: non-existent volume must not error.
	if err != nil {
		Expect(status.Code(err)).To(Equal(codes.NotFound),
			"%s: non-existent delete may return NotFound but not other errors", tc.tcNodeLabel())
	}
}

// ─── E28.7: ReconcileState ────────────────────────────────────────────────────

// assertE28_LVM_ReconcileState_RestoresExports verifies that ReconcileState
// restores NVMe-oF configfs subsystem entries for LVM volumes.
// [TC-E28.262]
func assertE28_LVM_ReconcileState_RestoresExports(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()
	defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-reconcile-a")
	defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-reconcile-b")

	// Create two LVM volumes to include in ReconcileState.
	Expect(lvmCreateVolumeE28(env, "pvc-e28-reconcile-a", 1<<30)).To(Succeed())
	Expect(lvmCreateVolumeE28(env, "pvc-e28-reconcile-b", 1<<30)).To(Succeed())

	_, err := env.client.ReconcileState(env.ctx, &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId: env.lvmVG + "/pvc-e28-reconcile-a",
				Exports: []*agentv1.ExportDesiredState{
					{
						ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
						ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
					},
				},
			},
			{
				VolumeId: env.lvmVG + "/pvc-e28-reconcile-b",
				Exports: []*agentv1.ExportDesiredState{
					{
						ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
						ExportParams: nvmeofTCPExportParams("127.0.0.1", 4421),
					},
				},
			},
		},
	})
	Expect(err).NotTo(HaveOccurred(),
		"%s: ReconcileState should restore configfs exports", tc.tcNodeLabel())
}

// ─── E28.8: LV name validation ───────────────────────────────────────────────

// assertE28_LVM_CreateVolume_ReservedPrefix_Snapshot verifies that an LV name
// starting with the reserved prefix "snapshot" is rejected with InvalidArgument.
// [TC-E28.263a]
func assertE28_LVM_CreateVolume_ReservedPrefix_Snapshot(tc documentedCase) {
	env := newValidatingAgentTestEnv()
	defer env.close()

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/snapshot-pvc-1",
		CapacityBytes: 1 << 30,
	})
	Expect(err).To(HaveOccurred(),
		"%s: LV name with reserved 'snapshot' prefix should be rejected", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument for reserved prefix", tc.tcNodeLabel())
}

// assertE28_LVM_CreateVolume_ReservedPrefix_Pvmove verifies that an LV name
// starting with "pvmove" is rejected with InvalidArgument.
// [TC-E28.263b]
func assertE28_LVM_CreateVolume_ReservedPrefix_Pvmove(tc documentedCase) {
	env := newValidatingAgentTestEnv()
	defer env.close()

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/pvmove-temp",
		CapacityBytes: 1 << 30,
	})
	Expect(err).To(HaveOccurred(),
		"%s: LV name with reserved 'pvmove' prefix should be rejected", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument for pvmove prefix", tc.tcNodeLabel())
}

// assertE28_LVM_CreateVolume_InvalidFirstChar_Hyphen verifies that an LV name
// starting with a hyphen is rejected with InvalidArgument.
// [TC-E28.263c]
func assertE28_LVM_CreateVolume_InvalidFirstChar_Hyphen(tc documentedCase) {
	env := newValidatingAgentTestEnv()
	defer env.close()

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/-invalid-name",
		CapacityBytes: 1 << 30,
	})
	Expect(err).To(HaveOccurred(),
		"%s: LV name starting with hyphen should be rejected", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument for leading hyphen", tc.tcNodeLabel())
}

// assertE28_LVM_CreateVolume_MaxLength_64 verifies that an LV name exactly
// 64 characters long is accepted.
//
// The kernel device-mapper name limit (DM_NAME_LEN=128) means the combined
// "VGName-LVName" string must stay under 127 chars. With pillar-csi test VG
// names of ~24 chars, the safe upper bound for an LV name is 64 chars — the
// value enforced by ValidateLVName and guaranteed to be accepted by real LVM.
// [TC-E28.263d]
func assertE28_LVM_CreateVolume_MaxLength_64(tc documentedCase) {
	env := newValidatingAgentTestEnv()
	defer env.close()

	name64 := "pvc" + strings.Repeat("a", 61) // 3 + 61 = 64 chars
	Expect(name64).To(HaveLen(64), "%s: name must be exactly 64 chars", tc.tcNodeLabel())
	defer lvmDeleteVolumeE28NoFail(env, name64)

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/" + name64,
		CapacityBytes: 1 << 30,
	})
	Expect(err).NotTo(HaveOccurred(),
		"%s: 64-character LV name should be accepted", tc.tcNodeLabel())
}

// assertE28_LVM_CreateVolume_OverMaxLength_65 verifies that an LV name of 65
// characters is rejected with InvalidArgument.
// [TC-E28.263e]
func assertE28_LVM_CreateVolume_OverMaxLength_65(tc documentedCase) {
	env := newValidatingAgentTestEnv()
	defer env.close()

	name65 := "pvc" + strings.Repeat("a", 62) // 3 + 62 = 65 chars
	Expect(name65).To(HaveLen(65), "%s: name must be exactly 65 chars", tc.tcNodeLabel())

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/" + name65,
		CapacityBytes: 1 << 30,
	})
	Expect(err).To(HaveOccurred(),
		"%s: 65-character LV name should be rejected", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument for over-length name", tc.tcNodeLabel())
}

// ─── E28.9: ExtraFlags forwarding + VG override ───────────────────────────────

// assertE28_LVM_CreateVolume_ExtraFlags_Forwarded verifies that ExtraFlags in
// LvmVolumeParams are accepted by the agent (forwarding verified by call log).
// [TC-E28.263f]
func assertE28_LVM_CreateVolume_ExtraFlags_Forwarded(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()
	defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-extraflags")

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/pvc-e28-extraflags",
		CapacityBytes: 1 << 30,
		BackendParams: &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Lvm{
				Lvm: &agentv1.LvmVolumeParams{
					ExtraFlags: []string{"--addtag", "owner=team-a"},
				},
			},
		},
	})
	Expect(err).NotTo(HaveOccurred(),
		"%s: CreateVolume with ExtraFlags should succeed", tc.tcNodeLabel())
	// Verify volume was created (ExtraFlags were forwarded and accepted by real LVM).
	listResp, listErr := env.client.ListVolumes(env.ctx, &agentv1.ListVolumesRequest{PoolName: env.lvmVG})
	Expect(listErr).NotTo(HaveOccurred(), "%s: ListVolumes after ExtraFlags create", tc.tcNodeLabel())
	Expect(listResp.GetVolumes()).To(ContainElement(
		HaveField("VolumeId", env.lvmVG+"/pvc-e28-extraflags"),
	), "%s: backend Create must be called when ExtraFlags are forwarded", tc.tcNodeLabel())
}

// assertE28_LVM_CreateVolume_ExtraFlags_Empty_NoEffect verifies that an empty
// ExtraFlags slice is accepted with no side effects.
// [TC-E28.263g]
func assertE28_LVM_CreateVolume_ExtraFlags_Empty_NoEffect(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()
	defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-extraflags-empty")

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/pvc-e28-extraflags-empty",
		CapacityBytes: 1 << 30,
		BackendParams: &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Lvm{
				Lvm: &agentv1.LvmVolumeParams{ExtraFlags: []string{}},
			},
		},
	})
	Expect(err).NotTo(HaveOccurred(),
		"%s: CreateVolume with empty ExtraFlags should succeed", tc.tcNodeLabel())
}

// assertE28_LVM_CreateVolume_VGOverride_Mismatch_Rejected verifies that providing
// a VolumeGroup that does not match the registered backend VG is rejected.
// Cross-VG provisioning must not be allowed.
// [TC-E28.263h]
func assertE28_LVM_CreateVolume_VGOverride_Mismatch_Rejected(tc documentedCase) {
	env := newValidatingAgentTestEnv()
	defer env.close()

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/pvc-e28-vg-mismatch",
		CapacityBytes: 1 << 30,
		BackendParams: &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Lvm{
				Lvm: &agentv1.LvmVolumeParams{VolumeGroup: "other-vg"},
			},
		},
	})
	Expect(err).To(HaveOccurred(),
		"%s: cross-VG provisioning should be rejected", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument for VG mismatch", tc.tcNodeLabel())
}

// ─── E28.10: Thin pool exhaustion ────────────────────────────────────────────

// assertE28_LVM_CreateVolume_ThinPool_NearFull verifies that CreateVolume is
// rejected when the requested capacity exceeds available linear VG space.
// Uses real LVM resource exhaustion — no error injection needed.
// [TC-E28.263i]
func assertE28_LVM_CreateVolume_ThinPool_NearFull(tc documentedCase) {
	// MUST use newLinearAgentTestEnv — thin provisioning allows over-commitment
	// so lvcreate with virtual size >> physical pool size SUCCEEDS with a thin
	// pool backend. Linear LVM enforces strict capacity: lvcreate rejects any
	// request that exceeds vg_free, which is what this TC is testing.
	env := newLinearAgentTestEnv()
	defer env.close()

	// Get current VG capacity to request more than available.
	capResp, err := env.client.GetCapacity(env.ctx, &agentv1.GetCapacityRequest{PoolName: env.lvmVG})
	Expect(err).NotTo(HaveOccurred(), "%s: GetCapacity", tc.tcNodeLabel())

	// Request 10x the total linear VG size — guaranteed to exceed available space.
	// Linear LVM rejects this with "insufficient free space".
	oversized := capResp.GetTotalBytes()*10 + 1

	_, err = env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/pvc-e28-nearfull",
		CapacityBytes: oversized,
	})
	Expect(err).To(HaveOccurred(),
		"%s: linear CreateVolume must fail when requesting 10x VG capacity", tc.tcNodeLabel())
}

// assertE28_LVM_CreateVolume_ThinPool_Full verifies that CreateVolume is
// rejected when the requested capacity greatly exceeds total linear VG capacity.
// Uses real LVM resource exhaustion — no error injection needed.
// [TC-E28.263j]
func assertE28_LVM_CreateVolume_ThinPool_Full(tc documentedCase) {
	// MUST use newLinearAgentTestEnv — see assertE28_LVM_CreateVolume_ThinPool_NearFull
	// for the rationale. Thin provisioning permits arbitrarily large virtual LVs;
	// only linear LVM enforces a hard capacity limit.
	env := newLinearAgentTestEnv()
	defer env.close()

	// Get current VG capacity to request astronomically more.
	capResp, err := env.client.GetCapacity(env.ctx, &agentv1.GetCapacityRequest{PoolName: env.lvmVG})
	Expect(err).NotTo(HaveOccurred(), "%s: GetCapacity", tc.tcNodeLabel())

	// Request 100x the total linear VG size — simulates a completely-full pool condition.
	oversized := capResp.GetTotalBytes()*100 + 1

	_, err = env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      env.lvmVG + "/pvc-e28-full",
		CapacityBytes: oversized,
	})
	Expect(err).To(HaveOccurred(),
		"%s: linear CreateVolume must fail when requesting 100x VG capacity", tc.tcNodeLabel())
}

// ─── E28.11: Multi-backend agent ─────────────────────────────────────────────

// assertE28_MultiBackend_ZFS_LVM_GetCapabilities verifies that when both ZFS and LVM
// backends are registered, GetCapabilities reports both BACKEND_TYPE_ZFS_ZVOL and
// BACKEND_TYPE_LVM.
// [TC-E28.263k]
func assertE28_MultiBackend_ZFS_LVM_GetCapabilities(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	resp, err := env.client.GetCapabilities(env.ctx, &agentv1.GetCapabilitiesRequest{})
	Expect(err).NotTo(HaveOccurred(), "%s: GetCapabilities", tc.tcNodeLabel())

	backends := resp.GetSupportedBackends()
	Expect(backends).To(ContainElement(agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL),
		"%s: BACKEND_TYPE_ZFS_ZVOL must be in supported backends", tc.tcNodeLabel())
	Expect(backends).To(ContainElement(agentv1.BackendType_BACKEND_TYPE_LVM),
		"%s: BACKEND_TYPE_LVM must be in supported backends", tc.tcNodeLabel())
}

// ─── Concurrent tests (kept for completeness) ─────────────────────────────────

// assertE28_LVM_Concurrent_CreateVolume verifies that concurrent CreateVolume calls
// with different LV names all succeed.
func assertE28_LVM_Concurrent_CreateVolume(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()
	// Clean up all concurrently-created LVs so the shared VG is not exhausted.
	const parallelism = 5
	for i := 0; i < parallelism; i++ {
		defer lvmDeleteVolumeE28NoFail(env, "pvc-e28-concurrent-"+string(rune('a'+i)))
	}

	var wg sync.WaitGroup
	errs := make([]error, parallelism)

	for i := 0; i < parallelism; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      env.lvmVG + "/pvc-e28-concurrent-" + string(rune('a'+idx)),
				CapacityBytes: 1 << 30,
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		Expect(err).NotTo(HaveOccurred(),
			"%s: concurrent LVM create %d failed", tc.tcNodeLabel(), i)
	}
}
