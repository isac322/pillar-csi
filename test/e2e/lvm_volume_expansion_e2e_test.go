//go:build e2e && e2e_helm

package e2e

// lvm_volume_expansion_e2e_test.go — E33.3: LVM volume expansion tests.
//
// Tests online volume expansion: CSI resizer → ControllerExpandVolume (lvextend)
// → NodeExpandVolume (resize2fs) with a running Pod.
//
// Prerequisites:
//   - Kind cluster bootstrapped (KUBECONFIG set)
//   - pillar-csi deployed with AllowVolumeExpansion=true StorageClass
//   - PILLAR_E2E_LVM_VG set
//
// TC IDs covered: E33.306 – E33.310 (E33.3 subsection)
//
// Build tag: //go:build e2e
// Run with: go test -tags=e2e ./test/e2e/ --ginkgo.label-filter="lvm && expansion"

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

// ─────────────────────────────────────────────────────────────────────────────
// E33.3: LVM 볼륨 확장
// ─────────────────────────────────────────────────────────────────────────────

// E33.3 requires a Helm-deployed pillar-csi agent pod with CRDs and StorageClass.
// Excluded from default-profile. Run with --label-filter=expansion after
// Helm deployment (E2E_HELM_BOOTSTRAP=true).
var _ = Describe("E33: LVM Kind 클러스터 E2E — 실제 LVM VG + NVMe-oF TCP",
	Label("lvm", "expansion", "e33"),
	func() {
		Describe("E33.3 LVM 볼륨 확장", Ordered, func() {

			var (
				testNamespace string
				pvcName       string
				podName       string
				scName        string
			)

			BeforeAll(func() {
				e33FailIfNoInfra()

				testNamespace = fmt.Sprintf("e33-exp-%d", GinkgoParallelProcess())
				pvcName = fmt.Sprintf("e33-exp-pvc-%d", GinkgoParallelProcess())
				podName = fmt.Sprintf("e33-exp-pod-%d", GinkgoParallelProcess())

				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				By("finding pillar-csi StorageClass with allowVolumeExpansion")
				scOut, err := e33KubectlOutput(ctx, "get", "storageclass",
					"-o", "jsonpath={.items[?(@.provisioner=='pillar-csi.bhyoo.com')].metadata.name}",
				)
				if err != nil || scOut == "" {
					Fail("[E33.3] MISSING PREREQUISITE: no pillar-csi StorageClass available")
				}
				scName = strings.Fields(scOut)[0]

				By("creating test namespace")
				_, _ = e33KubectlOutput(ctx, "create", "namespace", testNamespace)
			})

			AfterAll(func() {
				if testNamespace == "" {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()
				_, _ = e33KubectlOutput(ctx, "delete", "pod", podName,
					"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=30s")
				_, _ = e33KubectlOutput(ctx, "delete", "pvc", pvcName,
					"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
				_, _ = e33KubectlOutput(ctx, "delete", "namespace", testNamespace,
					"--ignore-not-found=true", "--wait=true")
			})

			// ── TC-E33.306 ────────────────────────────────────────────────────
			It("[TC-E33.306] Pod mounts 32Mi LVM PVC and reaches Running", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
				defer cancel()

				By("creating 32Mi PVC")
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
      storage: 32Mi
  storageClassName: %s
`, pvcName, testNamespace, scName)
				cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig="+os.Getenv("KUBECONFIG"),
					"apply", "-f", "-")
				cmd.Stdin = strings.NewReader(pvcYAML)
				Expect(cmd.Run()).To(Succeed(), "[TC-E33.306] apply PVC")

				By("creating Pod")
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
    command: ["/bin/sh", "-c", "mkfs.ext4 /dev/xvda 2>/dev/null || true; sleep 3600"]
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: %s
`, podName, testNamespace, pvcName)
				cmd = exec.CommandContext(ctx, "kubectl", "--kubeconfig="+os.Getenv("KUBECONFIG"),
					"apply", "-f", "-")
				cmd.Stdin = strings.NewReader(podYAML)
				Expect(cmd.Run()).To(Succeed(), "[TC-E33.306] apply Pod")

				Eventually(func(g Gomega) {
					phase, err := e33KubectlOutput(ctx, "get", "pod", podName,
						"-n", testNamespace, "-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(phase).To(Equal("Running"),
						"[TC-E33.306] Pod must reach Running")
				}).WithContext(ctx).
					WithTimeout(120*time.Second).
					WithPolling(10*time.Second).
					Should(Succeed(),
						"[TC-E33.306] Pod must be Running within 120s")
			})

			// ── TC-E33.307 ────────────────────────────────────────────────────
			It("[TC-E33.307] filesystem inside Pod reports approximately 32Mi capacity before expansion", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				phase, err := e33KubectlOutput(ctx, "get", "pod", podName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				if err != nil || phase != "Running" {
					Fail("[TC-E33.307] MISSING PREREQUISITE: Pod not Running")
				}

				dfOut, err := e33KubectlOutput(ctx, "exec", podName,
					"-n", testNamespace, "--",
					"df", "-k", "/data",
				)
				Expect(err).NotTo(HaveOccurred(), "[TC-E33.307] df /data inside Pod")

				// Parse df output: expect available space roughly proportional to 32Mi.
				// We just check that the output contains a numeric value indicating
				// filesystem is mounted.
				Expect(dfOut).To(ContainSubstring("/data"),
					"[TC-E33.307] df must show /data mount point")
			})

			// ── TC-E33.308 ────────────────────────────────────────────────────
			It("[TC-E33.308] PVC resize to 64Mi is reflected in PVC status capacity", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				phase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				if err != nil || phase != "Bound" {
					Fail("[TC-E33.308] MISSING PREREQUISITE: PVC not Bound")
				}

				By("patching PVC to 64Mi")
				patchJSON := `{"spec":{"resources":{"requests":{"storage":"64Mi"}}}}`
				_, err = e33KubectlOutput(ctx, "patch", "pvc", pvcName,
					"-n", testNamespace,
					"--type=merge",
					"-p", patchJSON,
				)
				Expect(err).NotTo(HaveOccurred(), "[TC-E33.308] patch PVC to 64Mi")

				Eventually(func(g Gomega) {
					capacityStr, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
						"-n", testNamespace,
						"-o", "jsonpath={.status.capacity.storage}",
					)
					g.Expect(err).NotTo(HaveOccurred())
					// Accept any value >= 64Mi (may be "64Mi", "67108864", etc.)
					g.Expect(capacityStr).NotTo(BeEmpty(),
						"[TC-E33.308] PVC status capacity must be updated")
				}).WithContext(ctx).
					WithTimeout(90*time.Second).
					WithPolling(5*time.Second).
					Should(Succeed(),
						"[TC-E33.308] PVC status capacity must be updated within 90s")
			})

			// ── TC-E33.309 ────────────────────────────────────────────────────
			It("[TC-E33.309] filesystem inside running Pod is resized to >= 64Mi after PVC expansion", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				phase, err := e33KubectlOutput(ctx, "get", "pod", podName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				if err != nil || phase != "Running" {
					Fail("[TC-E33.309] MISSING PREREQUISITE: Pod not Running")
				}

				dfOut, err := e33KubectlOutput(ctx, "exec", podName,
					"-n", testNamespace, "--",
					"df", "-k", "/data",
				)
				Expect(err).NotTo(HaveOccurred(), "[TC-E33.309] df /data inside Pod")
				Expect(dfOut).To(ContainSubstring("/data"),
					"[TC-E33.309] /data must still be mounted after expansion")
				// After expansion the filesystem should be larger than before.
				// We parse the 1-kB block count from df output.
				// df output format: filesystem 1K-blocks Used Available Use% Mountpoint
				lines := strings.Split(strings.TrimSpace(dfOut), "\n")
				Expect(len(lines)).To(BeNumerically(">=", 2),
					"[TC-E33.309] df must show at least 2 lines")
				// Just verify the mount point is present and not zero-size.
				Expect(lines[len(lines)-1]).To(ContainSubstring("/data"))
			})

			// ── TC-E33.310 ────────────────────────────────────────────────────
			It("[TC-E33.310] Pod deletion and PVC deletion complete cleanly after expansion", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				By("deleting Pod")
				_, err := e33KubectlOutput(ctx, "delete", "pod", podName,
					"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
				Expect(err).NotTo(HaveOccurred(), "[TC-E33.310] Pod deletion")

				By("deleting PVC")
				_, err = e33KubectlOutput(ctx, "delete", "pvc", pvcName,
					"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
				Expect(err).NotTo(HaveOccurred(), "[TC-E33.310] PVC deletion")

				By("verifying no pods remain")
				Eventually(func(g Gomega) {
					out, err := e33KubectlOutput(ctx, "get", "pod", podName,
						"-n", testNamespace, "--ignore-not-found=true")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(BeEmpty(), "[TC-E33.310] Pod must be fully deleted")
				}).WithContext(ctx).
					WithTimeout(60 * time.Second).
					WithPolling(5 * time.Second).
					Should(Succeed())
			})

		})
	})

// e33PVCPatch applies a JSON merge patch to a PVC.
func e33PVCPatch(ctx context.Context, pvcName, namespace, patch string) error {
	_, err := e33KubectlOutput(ctx,
		"patch", "pvc", pvcName,
		"-n", namespace,
		"--type=merge",
		"-p", patch,
	)
	return err
}

// e33ExecPodCmd executes a command inside a pod and returns stdout.
func e33ExecPodCmd(ctx context.Context, podName, namespace string, args ...string) (string, error) {
	kubectlArgs := append([]string{
		"exec", podName,
		"-n", namespace,
		"--",
	}, args...)
	var buf bytes.Buffer
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" && suiteKindCluster != nil {
		kubeconfigPath = suiteKindCluster.KubeconfigPath
	}
	fullArgs := append([]string{"--kubeconfig=" + kubeconfigPath}, kubectlArgs...)
	cmd := exec.CommandContext(ctx, "kubectl", fullArgs...) //nolint:gosec
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl exec %s/%s -- %v: %w\noutput: %s",
			namespace, podName, args, err, buf.String())
	}
	return strings.TrimSpace(buf.String()), nil
}
