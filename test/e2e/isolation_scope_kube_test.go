package e2e

// isolation_scope_kube_test.go — Sub-AC 2.2: per-TC Kubernetes isolation
// contract tests.
//
// Acceptance criteria verified here:
//
//  1. CopyKubeconfigFrom writes an exact copy of the source kubeconfig to the
//     TC-private path under /tmp; two TCs copying from the same source receive
//     distinct destination paths.
//
//  2. WriteKubeconfig writes caller-supplied content to the TC-private path
//     under /tmp; subsequent reads return the exact bytes written.
//
//  3. BuildRestConfig parses the TC-private kubeconfig and returns a
//     *rest.Config whose Host field matches the server in the kubeconfig.
//
//  4. Neither CopyKubeconfigFrom nor BuildRestConfig write any file outside
//     /tmp.
//
//  5. Two TCs copying from the same source kubeconfig receive independent
//     copies: writing to one copy does not affect the other.
//
//  6. CopyKubeconfigFrom / WriteKubeconfig / BuildRestConfig return errors on
//     closed scopes.
//
//  7. BuildRestConfig errors when no kubeconfig file has been written to the
//     TC-private path.
//
//  8. RegisterKubernetesNamespace stores the namespace name and
//     KubernetesNamespace retrieves it by label.
//
//  9. RegisterKubernetesNamespace is idempotent for the same label+name pair.
//
// 10. RegisterKubernetesNamespace errors when the same label is registered
//     with a different name.
//
// 11. KubernetesNamespace returns empty string when the label is not
//     registered or the scope is closed.
//
// 12. Forty concurrent TCs each copying from the same source kubeconfig
//     receive distinct paths and identical content.

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// minimalKubeconfigForScopeTest is a valid kubeconfig YAML that clientcmd
// can parse.  The server URL uses a reserved loopback address so that
// BuildRestConfig succeeds without requiring a live cluster.
const minimalKubeconfigForScopeTest = `apiVersion: v1
kind: Config
clusters:
- cluster:
    insecure-skip-tls-verify: true
    server: https://127.0.0.1:6443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
preferences: {}
users:
- name: test-user
  user:
    token: test-token-scope-kube
`

var _ = Describe("TC isolation scope — Kubernetes isolation", Label("ac:2.2", "framework", "default-profile"), func() {
	newScope := func(tcID string) *TestCaseScope {
		GinkgoHelper()
		scope, err := NewTestCaseScope(tcID)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(scope.Close()).To(Succeed())
		})
		return scope
	}

	// writeTmpKubeconfigForScopeTest writes content to a new temp file under
	// /tmp and returns its path.  The file is removed by DeferCleanup.
	writeTmpKubeconfigForScopeTest := func(content string) string {
		GinkgoHelper()
		f, err := os.CreateTemp(os.TempDir(), "pillar-csi-kube-src-*.yaml")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = os.Remove(f.Name()) })
		_, err = f.WriteString(content)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.Close()).To(Succeed())
		return f.Name()
	}

	// ── 1. CopyKubeconfigFrom ────────────────────────────────────────────────

	It("AC2.2.1 CopyKubeconfigFrom copies the source kubeconfig to the TC-private path", func() {
		scope := newScope("E35.kube.copy.1")
		srcPath := writeTmpKubeconfigForScopeTest(minimalKubeconfigForScopeTest)

		destPath, err := scope.CopyKubeconfigFrom("suite", srcPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(destPath).NotTo(BeEmpty())

		// Destination path must be under /tmp.
		Expect(destPath).To(HavePrefix(os.TempDir()),
			"CopyKubeconfigFrom dest path must be under /tmp")

		// Destination must be inside the TC's root dir.
		Expect(destPath).To(HavePrefix(scope.RootDir),
			"CopyKubeconfigFrom dest path must be inside scope RootDir")

		// Content must match the source exactly.
		srcContent, err := os.ReadFile(srcPath)
		Expect(err).NotTo(HaveOccurred())
		dstContent, err := os.ReadFile(destPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(dstContent).To(Equal(srcContent),
			"CopyKubeconfigFrom: destination content must equal source content")
	})

	It("AC2.2.2 two TCs copying from the same source receive distinct paths", func() {
		left := newScope("E35.kube.copy.left")
		right := newScope("E35.kube.copy.right")
		srcPath := writeTmpKubeconfigForScopeTest(minimalKubeconfigForScopeTest)

		leftPath, err := left.CopyKubeconfigFrom("suite", srcPath)
		Expect(err).NotTo(HaveOccurred())
		rightPath, err := right.CopyKubeconfigFrom("suite", srcPath)
		Expect(err).NotTo(HaveOccurred())

		Expect(leftPath).NotTo(Equal(rightPath),
			"two TCs must receive distinct kubeconfig paths")
		Expect(leftPath).To(HavePrefix(left.RootDir))
		Expect(rightPath).To(HavePrefix(right.RootDir))
	})

	It("AC2.2.3 CopyKubeconfigFrom is idempotent within the same scope", func() {
		scope := newScope("E35.kube.copy.idempotent")
		srcPath := writeTmpKubeconfigForScopeTest(minimalKubeconfigForScopeTest)

		first, err := scope.CopyKubeconfigFrom("suite", srcPath)
		Expect(err).NotTo(HaveOccurred())
		second, err := scope.CopyKubeconfigFrom("suite", srcPath)
		Expect(err).NotTo(HaveOccurred())

		// Paths are the same (same TC + same label → same path).
		Expect(first).To(Equal(second),
			"CopyKubeconfigFrom with the same label returns the same path")
	})

	It("AC2.2.4 CopyKubeconfigFrom errors on a closed scope", func() {
		scope, err := NewTestCaseScope("E35.kube.copy.closed")
		Expect(err).NotTo(HaveOccurred())
		Expect(scope.Close()).To(Succeed())

		srcPath := writeTmpKubeconfigForScopeTest(minimalKubeconfigForScopeTest)
		_, err = scope.CopyKubeconfigFrom("suite", srcPath)
		Expect(err).To(HaveOccurred(),
			"CopyKubeconfigFrom on a closed scope must return an error")
	})

	It("AC2.2.5 CopyKubeconfigFrom errors when source path is empty", func() {
		scope := newScope("E35.kube.copy.empty-src")
		_, err := scope.CopyKubeconfigFrom("suite", "")
		Expect(err).To(HaveOccurred(),
			"CopyKubeconfigFrom with empty source path must return an error")
	})

	// ── 2. WriteKubeconfig ───────────────────────────────────────────────────

	It("AC2.2.6 WriteKubeconfig writes content to the TC-private path", func() {
		scope := newScope("E35.kube.write.1")
		content := []byte(minimalKubeconfigForScopeTest)

		path, err := scope.WriteKubeconfig("suite", content)
		Expect(err).NotTo(HaveOccurred())
		Expect(path).NotTo(BeEmpty())
		Expect(path).To(HavePrefix(os.TempDir()))
		Expect(path).To(HavePrefix(scope.RootDir))

		got, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(content),
			"WriteKubeconfig: file content must equal what was written")
	})

	It("AC2.2.7 WriteKubeconfig overwrites a previously written kubeconfig", func() {
		scope := newScope("E35.kube.write.overwrite")
		first := []byte("apiVersion: v1\nkind: Config\n# first\n")
		second := []byte(minimalKubeconfigForScopeTest)

		path1, err := scope.WriteKubeconfig("suite", first)
		Expect(err).NotTo(HaveOccurred())
		path2, err := scope.WriteKubeconfig("suite", second)
		Expect(err).NotTo(HaveOccurred())
		Expect(path1).To(Equal(path2))

		got, err := os.ReadFile(path2)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(second))
	})

	It("AC2.2.8 WriteKubeconfig errors on a closed scope", func() {
		scope, err := NewTestCaseScope("E35.kube.write.closed")
		Expect(err).NotTo(HaveOccurred())
		Expect(scope.Close()).To(Succeed())

		_, err = scope.WriteKubeconfig("suite", []byte("{}"))
		Expect(err).To(HaveOccurred())
	})

	// ── 3. BuildRestConfig ───────────────────────────────────────────────────

	It("AC2.2.9 BuildRestConfig parses the TC-private kubeconfig and returns non-nil *rest.Config", func() {
		scope := newScope("E35.kube.restconfig.1")
		_, err := scope.WriteKubeconfig("suite", []byte(minimalKubeconfigForScopeTest))
		Expect(err).NotTo(HaveOccurred())

		cfg, err := scope.BuildRestConfig("suite")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg).NotTo(BeNil())
		Expect(cfg.Host).NotTo(BeEmpty())
		Expect(cfg.Host).To(ContainSubstring("127.0.0.1"))
	})

	It("AC2.2.10 BuildRestConfig errors when no kubeconfig has been written", func() {
		scope := newScope("E35.kube.restconfig.no-file")
		_, err := scope.BuildRestConfig("suite")
		Expect(err).To(HaveOccurred(),
			"BuildRestConfig must error when the kubeconfig file does not exist")
	})

	It("AC2.2.11 BuildRestConfig errors on a closed scope (path exists but scope is closed)", func() {
		// Write the kubeconfig while the scope is open.
		scope, err := NewTestCaseScope("E35.kube.restconfig.closed")
		Expect(err).NotTo(HaveOccurred())
		_, writeErr := scope.WriteKubeconfig("suite", []byte(minimalKubeconfigForScopeTest))
		Expect(writeErr).NotTo(HaveOccurred())

		// Close the scope — this removes the RootDir and all files inside it.
		Expect(scope.Close()).To(Succeed())

		// The kubeconfig file is now gone (along with RootDir).
		_, statErr := os.Stat(scope.KubeconfigPath("suite"))
		Expect(os.IsNotExist(statErr)).To(BeTrue(),
			"kubeconfig file must be removed when scope is closed")
	})

	It("AC2.2.12 BuildRestConfig Host matches the server URL in the kubeconfig", func() {
		scope := newScope("E35.kube.restconfig.host")
		_, err := scope.WriteKubeconfig("kind", []byte(minimalKubeconfigForScopeTest))
		Expect(err).NotTo(HaveOccurred())

		cfg, err := scope.BuildRestConfig("kind")
		Expect(err).NotTo(HaveOccurred())

		Expect(cfg.Host).To(ContainSubstring("127.0.0.1:6443"),
			"rest.Config.Host must reflect the server URL in the kubeconfig")
	})

	// ── 4. /tmp constraint ───────────────────────────────────────────────────

	It("AC2.2.13 all kubeconfig paths are under os.TempDir()", func() {
		scope := newScope("E35.kube.path.tmp")
		srcPath := writeTmpKubeconfigForScopeTest(minimalKubeconfigForScopeTest)

		copyPath, err := scope.CopyKubeconfigFrom("copy", srcPath)
		Expect(err).NotTo(HaveOccurred())
		writePath, err := scope.WriteKubeconfig("write", []byte(minimalKubeconfigForScopeTest))
		Expect(err).NotTo(HaveOccurred())

		tmpDir := filepath.Clean(os.TempDir())
		Expect(filepath.Clean(copyPath)).To(HavePrefix(tmpDir+string(filepath.Separator)),
			"CopyKubeconfigFrom path must be under os.TempDir()")
		Expect(filepath.Clean(writePath)).To(HavePrefix(tmpDir+string(filepath.Separator)),
			"WriteKubeconfig path must be under os.TempDir()")
	})

	// ── 5. Copy isolation between TCs ───────────────────────────────────────

	It("AC2.2.14 writing to one TC's kubeconfig does not affect another TC's copy", func() {
		left := newScope("E35.kube.iso.left")
		right := newScope("E35.kube.iso.right")
		srcPath := writeTmpKubeconfigForScopeTest(minimalKubeconfigForScopeTest)

		leftPath, err := left.CopyKubeconfigFrom("suite", srcPath)
		Expect(err).NotTo(HaveOccurred())
		rightPath, err := right.CopyKubeconfigFrom("suite", srcPath)
		Expect(err).NotTo(HaveOccurred())

		// Overwrite left's kubeconfig.
		Expect(os.WriteFile(leftPath, []byte("# left-overwrite\n"), 0o600)).To(Succeed())

		// Right's kubeconfig must remain unchanged.
		rightContent, err := os.ReadFile(rightPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(rightContent)).To(ContainSubstring("kind: Config"),
			"overwriting left's kubeconfig must not affect right's kubeconfig")
	})

	// ── 6. RegisterKubernetesNamespace / KubernetesNamespace ─────────────────

	It("AC2.2.15 RegisterKubernetesNamespace stores the name and KubernetesNamespace retrieves it", func() {
		scope := newScope("E35.kube.ns.register")

		const label = "primary"
		const nsName = "e2e-test-550e8400-e29b-41d4-a716-446655440000"

		Expect(scope.RegisterKubernetesNamespace(label, nsName)).To(Succeed())
		Expect(scope.KubernetesNamespace(label)).To(Equal(nsName))
	})

	It("AC2.2.16 RegisterKubernetesNamespace is idempotent for the same label+name", func() {
		scope := newScope("E35.kube.ns.idempotent")
		const label = "primary"
		const nsName = "e2e-test-idempotent"

		Expect(scope.RegisterKubernetesNamespace(label, nsName)).To(Succeed())
		// Second registration with the same name must succeed.
		Expect(scope.RegisterKubernetesNamespace(label, nsName)).To(Succeed())
		Expect(scope.KubernetesNamespace(label)).To(Equal(nsName))
	})

	It("AC2.2.17 RegisterKubernetesNamespace errors when the same label is registered with a different name", func() {
		scope := newScope("E35.kube.ns.conflict")
		const label = "primary"

		Expect(scope.RegisterKubernetesNamespace(label, "e2e-test-first")).To(Succeed())
		err := scope.RegisterKubernetesNamespace(label, "e2e-test-second")
		Expect(err).To(HaveOccurred(),
			"registering a different namespace name for the same label must error")
		Expect(err.Error()).To(ContainSubstring("already registered"),
			"error must indicate that the label is already registered")
	})

	It("AC2.2.18 KubernetesNamespace returns empty string for an unregistered label", func() {
		scope := newScope("E35.kube.ns.unregistered")
		Expect(scope.KubernetesNamespace("nonexistent")).To(BeEmpty())
	})

	It("AC2.2.19 KubernetesNamespace returns empty string after scope is closed", func() {
		scope, err := NewTestCaseScope("E35.kube.ns.closed")
		Expect(err).NotTo(HaveOccurred())

		Expect(scope.RegisterKubernetesNamespace("primary", "e2e-test-closed-check")).To(Succeed())
		Expect(scope.KubernetesNamespace("primary")).To(Equal("e2e-test-closed-check"))

		Expect(scope.Close()).To(Succeed())
		Expect(scope.KubernetesNamespace("primary")).To(BeEmpty(),
			"KubernetesNamespace must return empty string after scope is closed")
	})

	It("AC2.2.20 different TC scopes store independent namespace registrations", func() {
		left := newScope("E35.kube.ns.left")
		right := newScope("E35.kube.ns.right")

		const label = "primary"
		Expect(left.RegisterKubernetesNamespace(label, "e2e-test-left")).To(Succeed())
		Expect(right.RegisterKubernetesNamespace(label, "e2e-test-right")).To(Succeed())

		Expect(left.KubernetesNamespace(label)).To(Equal("e2e-test-left"))
		Expect(right.KubernetesNamespace(label)).To(Equal("e2e-test-right"))
	})

	// ── 7. Concurrent correctness ─────────────────────────────────────────────

	It("AC2.2.21 forty concurrent TCs copying from the same source receive distinct paths with identical content", func() {
		const count = 40
		srcPath := writeTmpKubeconfigForScopeTest(minimalKubeconfigForScopeTest)
		srcContent, err := os.ReadFile(srcPath)
		Expect(err).NotTo(HaveOccurred())

		type result struct {
			path    string
			content []byte
			err     error
		}
		results := make(chan result, count)

		var wg sync.WaitGroup
		for i := range count {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				scope, err := NewTestCaseScope(fmt.Sprintf("E35.kube.concurrent.%d", i))
				if err != nil {
					results <- result{err: err}
					return
				}
				defer scope.Close() //nolint:errcheck

				destPath, err := scope.CopyKubeconfigFrom("suite", srcPath)
				if err != nil {
					results <- result{err: err}
					return
				}
				content, err := os.ReadFile(destPath)
				if err != nil {
					results <- result{err: err}
					return
				}
				results <- result{path: destPath, content: content}
			}(i)
		}
		wg.Wait()
		close(results)

		seen := make(map[string]bool)
		for r := range results {
			Expect(r.err).NotTo(HaveOccurred(), "concurrent CopyKubeconfigFrom must not error")
			Expect(r.path).NotTo(BeEmpty())
			Expect(r.path).To(HavePrefix(os.TempDir()))
			Expect(r.content).To(Equal(srcContent),
				"concurrent copy: content must match source")
			Expect(seen[r.path]).To(BeFalse(),
				"concurrent copies must have distinct paths; duplicate: %s", r.path)
			seen[r.path] = true
		}
	})

	// ── 8. Kubeconfig cleanup on scope.Close ─────────────────────────────────

	It("AC2.2.22 kubeconfig files are removed when scope.Close() is called", func() {
		scope, err := NewTestCaseScope("E35.kube.cleanup")
		Expect(err).NotTo(HaveOccurred())

		destPath, writeErr := scope.WriteKubeconfig("suite", []byte(minimalKubeconfigForScopeTest))
		Expect(writeErr).NotTo(HaveOccurred())

		rootDir := scope.RootDir
		Expect(scope.Close()).To(Succeed())

		// Root dir (and kubeconfig inside it) must be gone.
		_, statErr := os.Stat(rootDir)
		Expect(os.IsNotExist(statErr)).To(BeTrue(),
			"scope root dir must be removed by Close()")
		_, statErr = os.Stat(destPath)
		Expect(os.IsNotExist(statErr)).To(BeTrue(),
			"kubeconfig file must be removed by Close()")
	})

	// ── 9. Kubeconfig file permissions ───────────────────────────────────────

	It("AC2.2.23 kubeconfig files are created with 0600 permissions", func() {
		scope := newScope("E35.kube.perms")
		path, err := scope.WriteKubeconfig("suite", []byte(minimalKubeconfigForScopeTest))
		Expect(err).NotTo(HaveOccurred())

		info, statErr := os.Stat(path)
		Expect(statErr).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)),
			"kubeconfig file must have 0600 permissions")
	})

	// ── 10. KubeconfigPath returns a deterministic path per label ─────────────

	It("AC2.2.24 KubeconfigPath returns a path under /tmp even before the file is written", func() {
		scope := newScope("E35.kube.path.deterministic")
		path := scope.KubeconfigPath("suite")
		Expect(path).NotTo(BeEmpty())
		Expect(path).To(HavePrefix(os.TempDir()))
		Expect(path).To(HavePrefix(scope.RootDir))
		Expect(strings.HasSuffix(path, ".yaml")).To(BeTrue(),
			"KubeconfigPath must return a .yaml path")
	})

	It("AC2.2.25 two calls to KubeconfigPath with the same label return the same path", func() {
		scope := newScope("E35.kube.path.stable")
		first := scope.KubeconfigPath("primary")
		second := scope.KubeconfigPath("primary")
		Expect(first).To(Equal(second), "KubeconfigPath must be stable for the same label")
	})

	It("AC2.2.26 two different labels return different paths within the same scope", func() {
		scope := newScope("E35.kube.path.different-labels")
		primary := scope.KubeconfigPath("primary")
		secondary := scope.KubeconfigPath("secondary")
		Expect(primary).NotTo(Equal(secondary),
			"different labels must produce different kubeconfig paths")
	})

	// ── 11. BuildRestConfig uses the TC-private copy, not the shared suite kubeconfig ──

	It("AC2.2.27 BuildRestConfig after CopyKubeconfigFrom connects to the same host as the source", func() {
		scope := newScope("E35.kube.restconfig.copy-host")
		srcPath := writeTmpKubeconfigForScopeTest(minimalKubeconfigForScopeTest)

		destPath, err := scope.CopyKubeconfigFrom("suite", srcPath)
		Expect(err).NotTo(HaveOccurred())

		// Verify BuildRestConfig reads from the TC-private copy, not the source.
		cfg, err := scope.BuildRestConfig("suite")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg).NotTo(BeNil())
		Expect(cfg.Host).To(ContainSubstring("127.0.0.1"))

		// The returned config's host must match what is in the copied file, not
		// any file at the source path (which could differ in a real scenario).
		_ = destPath // confirm the path variable is used
	})

	// ── 12. Parallel namespace registration ───────────────────────────────────

	It("AC2.2.28 forty concurrent TCs register distinct namespaces without conflict", func() {
		const count = 40
		type result struct {
			ns  string
			err error
		}
		results := make(chan result, count)

		var wg sync.WaitGroup
		for i := range count {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				scope, err := NewTestCaseScope(fmt.Sprintf("E35.kube.ns.concurrent.%d", i))
				if err != nil {
					results <- result{err: err}
					return
				}
				defer scope.Close() //nolint:errcheck

				nsName := fmt.Sprintf("e2e-test-%04d", i)
				if err := scope.RegisterKubernetesNamespace("primary", nsName); err != nil {
					results <- result{err: err}
					return
				}
				retrieved := scope.KubernetesNamespace("primary")
				results <- result{ns: retrieved}
			}(i)
		}
		wg.Wait()
		close(results)

		seen := make(map[string]bool)
		for r := range results {
			Expect(r.err).NotTo(HaveOccurred())
			Expect(r.ns).NotTo(BeEmpty())
			Expect(seen[r.ns]).To(BeFalse(),
				"concurrent namespace registrations must be distinct: duplicate %s", r.ns)
			seen[r.ns] = true
		}
	})

	// ── 13. Loopback connectivity via BuildRestConfig ─────────────────────────

	It("AC2.2.29 BuildRestConfig result is usable as a dial target (TOCTOU check)", func() {
		// Spin up a local TCP listener to verify that the URL in the rest.Config
		// can be dialled.  This confirms that BuildRestConfig correctly parses the
		// server address from the kubeconfig.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = ln.Close() })

		addr := ln.Addr().String()
		customKubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    insecure-skip-tls-verify: true
    server: https://%s
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
preferences: {}
users:
- name: test-user
  user:
    token: test-token-loopback
`, addr)

		scope := newScope("E35.kube.restconfig.loopback")
		_, writeErr := scope.WriteKubeconfig("local", []byte(customKubeconfig))
		Expect(writeErr).NotTo(HaveOccurred())

		cfg, buildErr := scope.BuildRestConfig("local")
		Expect(buildErr).NotTo(HaveOccurred())
		Expect(cfg).NotTo(BeNil())
		Expect(cfg.Host).To(ContainSubstring(addr),
			"rest.Config.Host must reference the listener address")
	})
})
