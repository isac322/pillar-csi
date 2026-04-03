package e2e

import (
	"os"
	"path/filepath"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Per-TC baseline setup", Label("ac:2", "framework", "default-profile"), func() {
	buildPlan := func(setupSeq int) TestCaseBaselinePlan {
		return TestCaseBaselinePlan{
			TempDirs:    []string{"workspace"},
			Kubeconfigs: []string{"kind"},
			BackendObjects: []BackendFixturePlan{
				{Kind: "zfs", Label: "pool"},
			},
			LoopbackPorts: []string{"agent"},
			Seed: func(baseline *TestCaseBaseline) error {
				if err := os.WriteFile(
					filepath.Join(baseline.TempDir("workspace"), "sentinel.txt"),
					[]byte("clean"),
					0o600,
				); err != nil {
					return err
				}
				if err := os.WriteFile(
					filepath.Join(baseline.TempDir("workspace"), "setup-seq.txt"),
					[]byte(strconv.Itoa(setupSeq)),
					0o600,
				); err != nil {
					return err
				}
				if err := os.WriteFile(
					baseline.Kubeconfig("kind"),
					[]byte("apiVersion: v1\nkind: Config\n"),
					0o600,
				); err != nil {
					return err
				}
				return os.WriteFile(
					baseline.BackendObject("zfs", "pool").Path("status"),
					[]byte("baseline"),
					0o600,
				)
			},
		}
	}

	Describe("UsePerTestCaseSetup", Ordered, func() {
		var (
			setupCalls      int
			previousRootDir string
			previousLease   *PortLease
		)

		tc := UsePerTestCaseSetup("E2.1", func(scope *TestCaseScope) (*TestCaseBaseline, error) {
			setupCalls++
			return BuildTestCaseBaseline(scope, buildPlan(setupCalls))
		})

		It("AC2.1 recreates a clean baseline before the first TC body runs", func() {
			Expect(setupCalls).To(Equal(1))

			baseline := tc.Baseline()
			Expect(baseline).NotTo(BeNil())
			Expect(tc.Scope()).NotTo(BeNil())

			previousRootDir = tc.Scope().RootDir
			previousLease = baseline.Port("agent")

			sentinelPath := filepath.Join(baseline.TempDir("workspace"), "sentinel.txt")
			seqPath := filepath.Join(baseline.TempDir("workspace"), "setup-seq.txt")
			backendStatusPath := baseline.BackendObject("zfs", "pool").Path("status")

			content, err := os.ReadFile(sentinelPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("clean"))

			content, err = os.ReadFile(seqPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("1"))

			content, err = os.ReadFile(backendStatusPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("baseline"))

			content, err = os.ReadFile(baseline.Kubeconfig("kind"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(ContainSubstring("kind: Config"))

			Expect(os.WriteFile(sentinelPath, []byte("dirty"), 0o600)).To(Succeed())
			Expect(os.WriteFile(
				filepath.Join(baseline.TempDir("workspace"), "stale.txt"),
				[]byte("stale"),
				0o600,
			)).To(Succeed())
			Expect(os.WriteFile(backendStatusPath, []byte("dirty"), 0o600)).To(Succeed())
		})

		It("AC2.2 reruns setup for the next TC instead of depending on prior state", func() {
			Expect(setupCalls).To(Equal(2))
			Expect(tc.Scope()).NotTo(BeNil())
			Expect(tc.Scope().RootDir).NotTo(Equal(previousRootDir))

			_, err := os.Stat(previousRootDir)
			Expect(os.IsNotExist(err)).To(BeTrue())

			baseline := tc.Baseline()
			sentinelPath := filepath.Join(baseline.TempDir("workspace"), "sentinel.txt")
			backendStatusPath := baseline.BackendObject("zfs", "pool").Path("status")

			content, err := os.ReadFile(sentinelPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("clean"))

			content, err = os.ReadFile(filepath.Join(baseline.TempDir("workspace"), "setup-seq.txt"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("2"))

			content, err = os.ReadFile(backendStatusPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("baseline"))

			_, err = os.Stat(filepath.Join(baseline.TempDir("workspace"), "stale.txt"))
			Expect(os.IsNotExist(err)).To(BeTrue())

			if previousLease != nil && !previousLease.Synthetic {
				Expect(previousLease.listener).To(BeNil())
			}
			Expect(baseline.Port("agent")).NotTo(BeNil())
		})
	})

	It("AC2.3 rebuilds managed baseline resources from scratch when setup is rerun in one scope", func() {
		scope, err := NewTestCaseScope("E2.2")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(scope.Close()).To(Succeed())
		})

		first, err := BuildTestCaseBaseline(scope, buildPlan(1))
		Expect(err).NotTo(HaveOccurred())

		workspace := first.TempDir("workspace")
		backend := first.BackendObject("zfs", "pool")
		firstLease := first.Port("agent")

		Expect(os.WriteFile(filepath.Join(workspace, "stale.txt"), []byte("stale"), 0o600)).To(Succeed())
		Expect(os.WriteFile(backend.Path("dirty"), []byte("dirty"), 0o600)).To(Succeed())
		Expect(os.WriteFile(first.Kubeconfig("kind"), []byte("dirty"), 0o600)).To(Succeed())

		second, err := BuildTestCaseBaseline(scope, buildPlan(2))
		Expect(err).NotTo(HaveOccurred())

		content, err := os.ReadFile(filepath.Join(second.TempDir("workspace"), "sentinel.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(content)).To(Equal("clean"))

		content, err = os.ReadFile(second.BackendObject("zfs", "pool").Path("status"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(content)).To(Equal("baseline"))

		content, err = os.ReadFile(second.Kubeconfig("kind"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(content)).To(ContainSubstring("kind: Config"))

		_, err = os.Stat(filepath.Join(second.TempDir("workspace"), "stale.txt"))
		Expect(os.IsNotExist(err)).To(BeTrue())

		_, err = os.Stat(second.BackendObject("zfs", "pool").Path("dirty"))
		Expect(os.IsNotExist(err)).To(BeTrue())

		if firstLease != nil && !firstLease.Synthetic {
			Expect(firstLease.listener).To(BeNil())
			Expect(second.Port("agent").listener).NotTo(BeNil())
		}
	})
})
