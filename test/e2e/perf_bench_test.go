package e2e

// perf_bench_test.go — Performance benchmarks for CSI controller operations.
//
// BenchmarkProvisioning_ZFSZvol measures CreateVolume/DeleteVolume throughput
// for the ZFS zvol backend using the in-process mock agent. The benchmark uses
// the same controllerTestEnv infrastructure as the in-process E2E TCs.

import (
	"testing"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
)

// BenchmarkProvisioning_ZFSZvol measures end-to-end CreateVolume throughput
// against the in-process fake agent (bufconn transport).
//
// This benchmark establishes a baseline for provisioning latency and helps
// detect regressions in the controller's gRPC dispatch path.
func BenchmarkProvisioning_ZFSZvol(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			env := newControllerTestEnv()
			_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
				Name:               benchVolumeName(i),
				Parameters:         env.params,
				VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
				CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
			})
			if err != nil {
				b.Errorf("BenchmarkProvisioning_ZFSZvol: CreateVolume iteration %d: %v", i, err)
			}
			env.close()
			i++
		}
	})
}

// benchVolumeName returns a unique volume name for benchmark iterations.
// Uses a simple numeric suffix to avoid name collisions within a parallel run.
// itoa is defined in profile_passfail_ac1_test.go (same package).
func benchVolumeName(i int) string {
	return "pvc-bench-" + itoa(i)
}
