package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Per-TC teardown enforcement", Label("ac:3", "framework", "default-profile"), func() {
	Describe("UsePerTestCaseSetup", Ordered, func() {
		var (
			currentPID          int
			previousRootDir     string
			previousMountPath   string
			previousVolumePath  string
			previousSnapshotDir string
			previousRecordPath  string
			previousPID         int
		)

		tc := UsePerTestCaseSetup("E17.1", func(scope *TestCaseScope) (*TestCaseBaseline, error) {
			baseline := newEmptyBaseline(scope)

			mountPath := scope.Path("mounts", "published")
			volumePath := scope.Path("volumes", "vol-1")
			snapshotDir := scope.Path("snapshots", "snap-1")
			recordPath := scope.Path("backend-records", "binding.json")

			if err := os.MkdirAll(filepath.Dir(mountPath), 0o755); err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Dir(volumePath), 0o755); err != nil {
				return nil, err
			}
			if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Dir(recordPath), 0o755); err != nil {
				return nil, err
			}

			if err := os.WriteFile(mountPath, []byte("mounted"), 0o600); err != nil {
				return nil, err
			}
			if err := os.WriteFile(volumePath, []byte("volume"), 0o600); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(snapshotDir, "meta.json"), []byte("{}"), 0o600); err != nil {
				return nil, err
			}
			if err := os.WriteFile(recordPath, []byte("{}"), 0o600); err != nil {
				return nil, err
			}

			cmd := exec.Command("sleep", "30")
			if err := cmd.Start(); err != nil {
				return nil, err
			}

			if err := scope.TrackMount("publish-target", MountResourceSpec{
				TargetPath: mountPath,
				Cleanup:    defaultPathCleanup(mountPath),
				IsPresent:  defaultPathPresenceProbe(mountPath),
			}); err != nil {
				_ = cmd.Process.Kill()
				return nil, err
			}
			if err := scope.TrackVolume("vol-1", PathResourceSpec{Path: volumePath}); err != nil {
				_ = cmd.Process.Kill()
				return nil, err
			}
			if err := scope.TrackSnapshot("snap-1", PathResourceSpec{Path: snapshotDir}); err != nil {
				_ = cmd.Process.Kill()
				return nil, err
			}
			if err := scope.TrackBackendRecord("binding-record", PathResourceSpec{Path: recordPath}); err != nil {
				_ = cmd.Process.Kill()
				return nil, err
			}
			if err := scope.TrackProcess("agent-helper", ProcessResourceSpec{Process: cmd.Process}); err != nil {
				_ = cmd.Process.Kill()
				return nil, err
			}

			currentPID = cmd.Process.Pid
			return baseline, nil
		})

		It("AC3.1 leaves tracked resources present during the TC body", func() {
			Expect(tc.Scope()).NotTo(BeNil())

			previousRootDir = tc.Scope().RootDir
			previousMountPath = tc.Scope().Path("mounts", "published")
			previousVolumePath = tc.Scope().Path("volumes", "vol-1")
			previousSnapshotDir = tc.Scope().Path("snapshots", "snap-1")
			previousRecordPath = tc.Scope().Path("backend-records", "binding.json")
			previousPID = currentPID

			_, err := os.Stat(previousMountPath)
			Expect(err).NotTo(HaveOccurred())
			_, err = os.Stat(previousVolumePath)
			Expect(err).NotTo(HaveOccurred())
			_, err = os.Stat(filepath.Join(previousSnapshotDir, "meta.json"))
			Expect(err).NotTo(HaveOccurred())
			_, err = os.Stat(previousRecordPath)
			Expect(err).NotTo(HaveOccurred())

			alive, err := processExists(previousPID)
			Expect(err).NotTo(HaveOccurred())
			Expect(alive).To(BeTrue())
		})

		It("AC3.2 removes tracked resources before the next TC begins", func() {
			Expect(tc.Scope()).NotTo(BeNil())
			Expect(tc.Scope().RootDir).NotTo(Equal(previousRootDir))

			// Sub-AC 5.3: UsePerTestCaseSetup now fires cleanup in a background
			// goroutine (CloseBackground) so DeferCleanup returns immediately.
			// Drain all pending background cleanups before asserting resource
			// absence so this test remains a correctness check rather than a
			// timing-dependent flake.
			Expect(DrainPendingCleanups(30 * time.Second)).To(Succeed())

			_, err := os.Stat(previousRootDir)
			Expect(os.IsNotExist(err)).To(BeTrue())
			_, err = os.Stat(previousMountPath)
			Expect(os.IsNotExist(err)).To(BeTrue())
			_, err = os.Stat(previousVolumePath)
			Expect(os.IsNotExist(err)).To(BeTrue())
			_, err = os.Stat(previousSnapshotDir)
			Expect(os.IsNotExist(err)).To(BeTrue())
			_, err = os.Stat(previousRecordPath)
			Expect(os.IsNotExist(err)).To(BeTrue())

			alive, err := processExists(previousPID)
			Expect(err).NotTo(HaveOccurred())
			Expect(alive).To(BeFalse())
		})
	})

	It("AC3.3 fails close when tracked resources remain after teardown verification", func() {
		ctx, err := StartTestCase("E17.2", func(scope *TestCaseScope) (*TestCaseBaseline, error) {
			mountPath := scope.Path("mounts", "leaked")
			volumePath := scope.Path("volumes", "leaked")
			snapshotDir := scope.Path("snapshots", "leaked")
			recordPath := scope.Path("backend-records", "leaked.json")

			Expect(os.MkdirAll(filepath.Dir(mountPath), 0o755)).To(Succeed())
			Expect(os.MkdirAll(filepath.Dir(volumePath), 0o755)).To(Succeed())
			Expect(os.MkdirAll(snapshotDir, 0o755)).To(Succeed())
			Expect(os.MkdirAll(filepath.Dir(recordPath), 0o755)).To(Succeed())
			Expect(os.WriteFile(mountPath, []byte("mounted"), 0o600)).To(Succeed())
			Expect(os.WriteFile(volumePath, []byte("volume"), 0o600)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(snapshotDir, "meta.json"), []byte("{}"), 0o600)).To(Succeed())
			Expect(os.WriteFile(recordPath, []byte("{}"), 0o600)).To(Succeed())

			Expect(scope.TrackMount("leaked-mount", MountResourceSpec{
				TargetPath: mountPath,
				Cleanup:    func() error { return nil },
				IsPresent:  defaultPathPresenceProbe(mountPath),
			})).To(Succeed())
			Expect(scope.TrackVolume("leaked-volume", PathResourceSpec{
				Path:    volumePath,
				Cleanup: func() error { return nil },
			})).To(Succeed())
			Expect(scope.TrackSnapshot("leaked-snapshot", PathResourceSpec{
				Path:    snapshotDir,
				Cleanup: func() error { return nil },
			})).To(Succeed())
			Expect(scope.TrackBackendRecord("leaked-record", PathResourceSpec{
				Path:    recordPath,
				Cleanup: func() error { return nil },
			})).To(Succeed())
			Expect(scope.TrackProcess("leaked-process", ProcessResourceSpec{
				PID:     424242,
				Cleanup: func() error { return nil },
				IsPresent: func() (bool, error) {
					return true, nil
				},
			})).To(Succeed())

			return newEmptyBaseline(scope), nil
		})
		Expect(err).NotTo(HaveOccurred())

		err = ctx.Close()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`mount "leaked-mount" remained after teardown`))
		Expect(err.Error()).To(ContainSubstring(`volume "leaked-volume" remained after teardown`))
		Expect(err.Error()).To(ContainSubstring(`snapshot "leaked-snapshot" remained after teardown`))
		Expect(err.Error()).To(ContainSubstring(`backend record "leaked-record" remained after teardown`))
		Expect(err.Error()).To(ContainSubstring(`process "leaked-process" remained after teardown`))
	})
})
