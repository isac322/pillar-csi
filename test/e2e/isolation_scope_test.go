package e2e

import (
	"net"
	"os"
	"path/filepath"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

var _ = Describe("TC isolation scope", Label("ac:1", "framework", "default-profile"), func() {
	newScope := func(tcID string) *TestCaseScope {
		GinkgoHelper()

		scope, err := NewTestCaseScope(tcID)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(scope.Close()).To(Succeed())
		})
		return scope
	}

	It("AC1.1 scopes names, namespaces, and temp roots to a single TC", func() {
		left := newScope("E1.1")
		right := newScope("E1.1")

		Expect(left.RootDir).To(HavePrefix("/tmp/"))
		Expect(right.RootDir).To(HavePrefix("/tmp/"))
		Expect(left.RootDir).NotTo(Equal(right.RootDir))

		leftNamespace := left.Namespace("controller")
		rightNamespace := right.Namespace("controller")
		Expect(leftNamespace).To(MatchRegexp(dnsLabelPattern.String()))
		Expect(rightNamespace).To(MatchRegexp(dnsLabelPattern.String()))
		Expect(len(leftNamespace)).To(BeNumerically("<=", 63))
		Expect(len(rightNamespace)).To(BeNumerically("<=", 63))
		Expect(leftNamespace).NotTo(Equal(rightNamespace))

		leftTmp, err := left.TempDir("state")
		Expect(err).NotTo(HaveOccurred())
		rightTmp, err := right.TempDir("state")
		Expect(err).NotTo(HaveOccurred())
		Expect(leftTmp).To(HavePrefix(left.RootDir))
		Expect(rightTmp).To(HavePrefix(right.RootDir))
		Expect(leftTmp).NotTo(Equal(rightTmp))

		info, err := os.Stat(leftTmp)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.IsDir()).To(BeTrue())

		Expect(left.KubeconfigPath("kind")).To(HavePrefix(filepath.Join(left.RootDir, "kubeconfig")))
		Expect(right.KubeconfigPath("kind")).To(HavePrefix(filepath.Join(right.RootDir, "kubeconfig")))
		Expect(left.KubeconfigPath("kind")).NotTo(Equal(right.KubeconfigPath("kind")))
	})

	It("AC1.2 allocates backend fixtures per TC instead of sharing mutable instances", func() {
		left := newScope("E33.1")
		right := newScope("E33.1")

		leftObject, err := left.BackendObject("zfs", "pool")
		Expect(err).NotTo(HaveOccurred())
		rightObject, err := right.BackendObject("zfs", "pool")
		Expect(err).NotTo(HaveOccurred())

		Expect(leftObject).NotTo(BeIdenticalTo(rightObject))
		Expect(leftObject.Name).NotTo(Equal(rightObject.Name))
		Expect(leftObject.RootDir).To(HavePrefix(left.RootDir))
		Expect(rightObject.RootDir).To(HavePrefix(right.RootDir))

		Expect(os.WriteFile(leftObject.Path("marker"), []byte("left"), 0o600)).To(Succeed())
		_, err = os.Stat(rightObject.Path("marker"))
		Expect(os.IsNotExist(err)).To(BeTrue())
	})

	It("AC1.3 reserves distinct loopback ports per TC scope", func() {
		left := newScope("E9.1")
		right := newScope("E9.1")

		leftLease, err := left.ReserveLoopbackPort("agent")
		Expect(err).NotTo(HaveOccurred())
		rightLease, err := right.ReserveLoopbackPort("agent")
		Expect(err).NotTo(HaveOccurred())

		Expect(leftLease.Port).NotTo(Equal(rightLease.Port))
		Expect(leftLease.Addr).To(HavePrefix("127.0.0.1:"))
		Expect(rightLease.Addr).To(HavePrefix("127.0.0.1:"))

		if leftLease.Synthetic || rightLease.Synthetic {
			return
		}

		conn, err := net.Dial("tcp", leftLease.Addr)
		Expect(err).NotTo(HaveOccurred())
		Expect(conn.Close()).To(Succeed())
	})

	It("AC1.4 tears down the TC root and releases loopback leases on close", func() {
		scope := newScope("E10.1")

		lease, err := scope.ReserveLoopbackPort("controller")
		Expect(err).NotTo(HaveOccurred())
		rootDir := scope.RootDir
		addr := lease.Addr

		Expect(scope.Close()).To(Succeed())

		_, err = os.Stat(rootDir)
		Expect(os.IsNotExist(err)).To(BeTrue())

		if lease.Synthetic {
			return
		}

		rebound, err := net.Listen("tcp", addr)
		Expect(err).NotTo(HaveOccurred())
		Expect(rebound.Close()).To(Succeed())
	})
})
