//go:build e2e

package e2e

// nfs_helpers_e2e.go — kubectl/agent helpers shared by the E37 NFS dataset
// lane (tc_nfs_dataset_e2e_test.go) and the non-test E71 network fixture
// (filesystem_network_fixture_e2e.go). They must live in a non-test file
// because fixtures are non-test sources and cannot reference _test.go-only
// symbols.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// agentExportfsPath is the agent image's relocated exportfs binary. The image
// moves it out of /usr/sbin because OpenZFS libshare unconditionally runs the
// hard-coded `/usr/sbin/exportfs -ra` when that path exists
// (lib/libshare/os/linux/nfs.c), wiping /var/lib/nfs/etab. Exportfs is
// therefore not on the agent container's PATH; invoke this absolute path.
const agentExportfsPath = "/usr/libexec/pillar-csi/exportfs"

func nfsMust(ctx context.Context, args ...string) string {
	out, err := nfsKubectl(ctx, "", args...)
	Expect(err).NotTo(HaveOccurred())
	return out
}

func nfsComponentPod(ctx context.Context, component, node string) string {
	args := []string{"-n", resolveHelmNamespace(), "get", "pods", "-l", "app.kubernetes.io/component=" + component}
	if node != "" {
		args = append(args, "--field-selector", "spec.nodeName="+node)
	}
	args = append(args, "-o", "jsonpath={.items[0].metadata.name}")
	pod := nfsMust(ctx, args...)
	Expect(pod).NotTo(BeEmpty())
	return pod
}

func nfsAgentExec(ctx context.Context, args ...string) string {
	pod := nfsComponentPod(ctx, "agent", os.Getenv(suiteBackendContainerEnvVar))
	return nfsMust(ctx, append([]string{"-n", resolveHelmNamespace(), "exec", pod, "-c", "agent", "--"}, args...)...)
}

func nfsNodeExec(ctx context.Context, node string, args ...string) (string, error) {
	pod := nfsComponentPod(ctx, "node", node)
	return nfsKubectl(ctx, "", append([]string{"-n", resolveHelmNamespace(), "exec", pod, "-c", "node", "--"}, args...)...)
}

// exportfs -v may wrap a long filesystem path onto its own line. Parse both
// forms and keep policies scoped to the exact physical dataset, never a
// sibling or the wildcard pseudoroot.
func nfsExportPolicies(exports, path string) map[string][]string {
	policies := map[string][]string{}
	current := ""
	for _, line := range strings.Split(exports, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.HasPrefix(fields[0], "/") {
			current = fields[0]
			fields = fields[1:]
		}
		if current != path {
			continue
		}
		for _, field := range fields {
			client, options, ok := strings.Cut(field, "(")
			if !ok {
				continue
			}
			policies[client] = strings.Split(strings.TrimSuffix(options, ")"), ",")
		}
	}
	return policies
}

// e33AgentGRPCClient creates an insecure gRPC client to the given address.
func e33AgentGRPCClient(ctx context.Context, addr string) (agentv1.AgentServiceClient, *grpc.ClientConn, error) {
	conn, err := grpc.DialContext(ctx, addr, //nolint:staticcheck
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),                 //nolint:staticcheck
		grpc.WithTimeout(10*time.Second), //nolint:staticcheck
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial agent at %s: %w", addr, err)
	}
	return agentv1.NewAgentServiceClient(conn), conn, nil
}
