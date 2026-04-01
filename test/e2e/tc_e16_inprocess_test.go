package e2e

// tc_e16_inprocess_test.go — Per-TC assertions for E16: Concurrent operations.

import (
	"fmt"
	"sync"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"

	csidrv "github.com/bhyoo/pillar-csi/internal/csi"
)

func assertE16_CreateVolume_SameName(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	const parallelism = 5
	var wg sync.WaitGroup
	errs := make([]error, parallelism)
	for i := range parallelism {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
				Name:               "pvc-e16-same-name",
				Parameters:         env.params,
				VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
				CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
			})
		}(i)
	}
	wg.Wait()
	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		}
	}
	Expect(successCount).To(BeNumerically(">=", 1),
		"%s: at least one concurrent create must succeed", tc.tcNodeLabel())
}

func assertE16_CreateVolume_DifferentNames(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	const parallelism = 5
	var wg sync.WaitGroup
	errs := make([]error, parallelism)
	for i := range parallelism {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
				Name:               fmt.Sprintf("pvc-e16-diff-%d", idx),
				Parameters:         env.params,
				VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
				CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		Expect(err).NotTo(HaveOccurred(), "%s: concurrent create %d failed", tc.tcNodeLabel(), i)
	}
}

func assertE16_DeleteVolume(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e16-concurrent-delete",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume for concurrent delete", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()

	const parallelism = 3
	var wg sync.WaitGroup
	errs := make([]error, parallelism)
	for i := range parallelism {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{VolumeId: volumeID})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		Expect(err).NotTo(HaveOccurred(), "%s: concurrent delete %d failed", tc.tcNodeLabel(), i)
	}
}

func assertE16_ExpandVolume(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e16-concurrent-expand",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume for concurrent expand", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()

	const parallelism = 3
	var wg sync.WaitGroup
	errs := make([]error, parallelism)
	for i := range parallelism {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = env.controller.ControllerExpandVolume(env.ctx, &csiapi.ControllerExpandVolumeRequest{
				VolumeId:      volumeID,
				CapacityRange: &csiapi.CapacityRange{RequiredBytes: 2 << 30},
			})
		}(i)
	}
	wg.Wait()
	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		}
	}
	Expect(successCount).To(BeNumerically(">=", 1), "%s: at least one expand succeeds", tc.tcNodeLabel())
}

func assertE16_NodeStage(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	const parallelism = 3
	var wg sync.WaitGroup
	errs := make([]error, parallelism)
	for i := range parallelism {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			suffix := fmt.Sprintf("%d", idx)
			vid := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e16-stage-" + suffix
			env.sm.ForceState(vid, csidrv.StateControllerPublished)
			_, errs[idx] = env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
				VolumeId:          vid,
				StagingTargetPath: env.stateDir + "/stage-" + suffix,
				VolumeCapability:  mountCapability("ext4"),
				VolumeContext:     makeNodeVolumeContext(),
			})
		}(i)
	}
	wg.Wait()
	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		}
	}
	Expect(successCount).To(BeNumerically(">=", 1), "%s: at least one NodeStageVolume succeeds", tc.tcNodeLabel())
}

func assertE16_NodePublish(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	const parallelism = 3
	var wg sync.WaitGroup
	errs := make([]error, parallelism)
	for i := range parallelism {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			suffix := fmt.Sprintf("%d", idx)
			vid := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e16-pub-" + suffix
			env.sm.ForceState(vid, csidrv.StateNodeStaged)
			_, errs[idx] = env.node.NodePublishVolume(env.ctx, &csiapi.NodePublishVolumeRequest{
				VolumeId:          vid,
				StagingTargetPath: env.stateDir + "/stage-" + suffix,
				TargetPath:        env.stateDir + "/target-" + suffix,
				VolumeCapability:  mountCapability("ext4"),
			})
		}(i)
	}
	wg.Wait()
	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		}
	}
	Expect(successCount).To(BeNumerically(">=", 1), "%s: at least one NodePublishVolume succeeds", tc.tcNodeLabel())
}

func assertE16_ControllerPublish(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e16-concurrent-pub",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()
	makeCSINodeWithNQN(env, "node-e16", "nqn.2026-01.io.example:node-e16")

	const parallelism = 3
	var wg sync.WaitGroup
	errs := make([]error, parallelism)
	for i := range parallelism {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
				VolumeId:         volumeID,
				NodeId:           "node-e16",
				VolumeCapability: mountCapability("ext4"),
			})
		}(i)
	}
	wg.Wait()
	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		}
	}
	Expect(successCount).To(BeNumerically(">=", 1), "%s: at least one ControllerPublish succeeds", tc.tcNodeLabel())
}
