/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command chartcontract checks a `helm template` render (read from stdin)
// against the controller-gen API contract: the CRDs in config/crd/bases and
// the ClusterRole generated from +kubebuilder:rbac markers. It decodes the
// objects instead of matching text, so it fails on any contract drift:
//
//   - a rendered CRD whose spec differs from the controller-gen CRD of the
//     same name, or that has no controller-gen source;
//   - a controller-gen CRD missing from an installCRDs=true render, or any CRD
//     rendered with installCRDs=false (-expect-no-crds);
//   - a CRD shortName that shadows a built-in Kubernetes shortName;
//   - a chart Role/ClusterRole granting a pillar-csi.bhyoo.com resource that no
//     controller-gen CRD serves (this breaks installs that apply CRDs separately);
//   - a controller-gen RBAC permission missing from the chart controller role;
//   - with -file-driver, the file CSI consumer contract: both CSIDriver
//     identities keep their fsGroupPolicy, socket/registrar routes stay paired,
//     and no container stacks Bidirectional mounts on nested host or mount
//     paths — the --nfs-export-root tree must be covered by exactly one
//     Bidirectional mount (a nested proxy root inherits the agent-state mount).
//
// Usage (see charts/pillar-csi/test_render.sh):
//
//	helm template ... | go run ./hack/chartcontract -controller-role <fullname> [-expect-no-crds]
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const pillarGroup = "pillar-csi.bhyoo.com"

// builtinShortNames are the shortNames served by a stock kube-apiserver. A CRD
// reusing one is unreachable through it (kubectl resolves the built-in first)
// and misleads users, e.g. `pv` for PersistentVolume.
var builtinShortNames = map[string]bool{
	"cm": true, "cs": true, "ep": true, "ev": true, "limits": true, "no": true, "ns": true,
	"po": true, "pv": true, "pvc": true, "quota": true, "rc": true, "sa": true, "svc": true,
	"crd": true, "crds": true, "ds": true, "deploy": true, "rs": true, "sts": true,
	"hpa": true, "cj": true, "csr": true, "ing": true, "netpol": true, "pdb": true,
	"pc": true, "sc": true,
}

type options struct {
	basesDir              string
	rolePath              string
	controllerRole        string
	expectNoCRDs          bool
	fileDriver            bool
	fileDriverBlockPolicy string
}

// renderedMount is one container volumeMount resolved only as far as the
// propagation contract needs: the backing volume name (to look up the host
// path), the container path, an optional subPath, and mountPropagation.
type renderedMount struct {
	volume      string
	mountPath   string
	subPath     string
	propagation string
}

type renderedContainer struct {
	name   string
	args   []string
	mounts []renderedMount
}

type renderedWorkload struct {
	kind       string
	name       string
	hostPaths  map[string]string // volume name → hostPath.path, hostPath volumes only
	containers []renderedContainer
}

type rendered struct {
	crds       []*apiextv1.CustomResourceDefinition
	roles      []*rbacv1.ClusterRole // Roles are decoded into ClusterRole: identical rules shape
	csiDrivers []*storagev1.CSIDriver
	workloads  []renderedWorkload
}

func main() {
	var o options
	flag.StringVar(&o.basesDir, "bases", "config/crd/bases", "controller-gen CRD directory")
	flag.StringVar(&o.rolePath, "role", "config/rbac/role.yaml", "controller-gen ClusterRole")
	flag.StringVar(&o.controllerRole, "controller-role", "", "name of the rendered controller ClusterRole")
	flag.BoolVar(&o.expectNoCRDs, "expect-no-crds", false, "the render used installCRDs=false")
	flag.BoolVar(&o.fileDriver, "file-driver", false, "check the opt-in file CSI consumer contract")
	flag.StringVar(&o.fileDriverBlockPolicy, "file-driver-block-policy", "File",
		"expected fsGroupPolicy of the existing block CSIDriver")
	flag.Parse()

	violations, err := run(o, os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chartcontract: %v\n", err)
		os.Exit(2)
	}
	if len(violations) > 0 {
		fmt.Fprintf(os.Stderr, "chart contract violations:\n  %s\n", strings.Join(violations, "\n  "))
		os.Exit(1)
	}
	fmt.Println("chart contract OK")
}

func run(o options, render io.Reader) ([]string, error) {
	if o.controllerRole == "" {
		return nil, errors.New("-controller-role is required")
	}
	gen, err := loadGeneratedCRDs(o.basesDir)
	if err != nil {
		return nil, fmt.Errorf("load generated CRDs: %w", err)
	}
	genRole := &rbacv1.ClusterRole{}
	err = decodeFile(o.rolePath, genRole)
	if err != nil {
		return nil, fmt.Errorf("decode generated RBAC: %w", err)
	}
	r, err := decodeRender(render)
	if err != nil {
		return nil, fmt.Errorf("decode chart render: %w", err)
	}

	v := checkCRDs(gen, r.crds, o.expectNoCRDs)
	v = append(v, checkRBAC(gen, genRole, r.roles, o.controllerRole)...)
	if o.fileDriver {
		v = append(v, checkFileDriver(r, o.fileDriverBlockPolicy)...)
	}
	sort.Strings(v)
	return v, nil
}

func loadGeneratedCRDs(dir string) (map[string]*apiextv1.CustomResourceDefinition, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no CRDs in %s", dir)
	}
	gen := make(map[string]*apiextv1.CustomResourceDefinition, len(files))
	for _, f := range files {
		crd := &apiextv1.CustomResourceDefinition{}
		decodeErr := decodeFile(f, crd)
		if decodeErr != nil {
			return nil, fmt.Errorf("load generated CRD %s: %w", f, decodeErr)
		}
		gen[crd.Name] = crd
	}
	return gen, nil
}

func decodeFile(path string, into any) error {
	b, err := os.ReadFile(path) //nolint:gosec // repo-relative generated manifest chosen by the caller
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	err = yaml.UnmarshalStrict(b, into)
	if err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func decodeRender(in io.Reader) (rendered, error) {
	var r rendered
	docs := utilyaml.NewYAMLReader(bufio.NewReader(in))
	for {
		doc, err := docs.Read()
		if errors.Is(err, io.EOF) {
			return r, nil
		}
		if err != nil {
			return r, fmt.Errorf("read render: %w", err)
		}
		var meta struct {
			Kind string `json:"kind"`
		}
		err = yaml.Unmarshal(doc, &meta)
		if err != nil {
			return r, fmt.Errorf("decode render document: %w", err)
		}
		err = appendRenderedDocument(&r, doc, meta.Kind)
		if err != nil {
			return r, fmt.Errorf("decode rendered %s: %w", meta.Kind, err)
		}
	}
}

func appendRenderedDocument(r *rendered, doc []byte, kind string) error {
	switch kind {
	case "CustomResourceDefinition":
		crd := &apiextv1.CustomResourceDefinition{}
		err := yaml.UnmarshalStrict(doc, crd)
		if err != nil {
			return fmt.Errorf("CRD: %w", err)
		}
		r.crds = append(r.crds, crd)
	case "ClusterRole", "Role":
		role := &rbacv1.ClusterRole{}
		err := yaml.Unmarshal(doc, role)
		if err != nil {
			return fmt.Errorf("%s %s: %w", kind, role.Name, err)
		}
		r.roles = append(r.roles, role)
	case "CSIDriver":
		driver := &storagev1.CSIDriver{}
		err := yaml.UnmarshalStrict(doc, driver)
		if err != nil {
			return fmt.Errorf("CSIDriver: %w", err)
		}
		r.csiDrivers = append(r.csiDrivers, driver)
	case "Deployment", "DaemonSet":
		workload, err := decodeWorkload(doc, kind)
		if err != nil {
			return err
		}
		r.workloads = append(r.workloads, workload)
	}
	return nil
}

func decodeWorkload(doc []byte, kind string) (renderedWorkload, error) {
	var object map[string]any
	err := yaml.Unmarshal(doc, &object)
	if err != nil {
		return renderedWorkload{}, fmt.Errorf("decode rendered %s: %w", kind, err)
	}
	metadata, err := requiredMapField(object, "metadata")
	if err != nil {
		return renderedWorkload{}, fmt.Errorf("rendered %s metadata: %w", kind, err)
	}
	spec, err := requiredMapField(object, "spec")
	if err != nil {
		return renderedWorkload{}, fmt.Errorf("rendered %s spec: %w", kind, err)
	}
	template, err := requiredMapField(spec, "template")
	if err != nil {
		return renderedWorkload{}, fmt.Errorf("rendered %s template: %w", kind, err)
	}
	podSpec, err := requiredMapField(template, "spec")
	if err != nil {
		return renderedWorkload{}, fmt.Errorf("rendered %s pod spec: %w", kind, err)
	}
	rawContainers, err := requiredSliceField(podSpec, "containers")
	if err != nil {
		return renderedWorkload{}, fmt.Errorf("rendered %s containers: %w", kind, err)
	}
	name, err := requiredStringField(metadata, "name")
	if err != nil {
		return renderedWorkload{}, fmt.Errorf("rendered %s metadata: %w", kind, err)
	}
	workload := renderedWorkload{kind: kind, name: name, hostPaths: map[string]string{}}
	err = decodeWorkloadVolumes(&workload, podSpec)
	if err != nil {
		return renderedWorkload{}, err
	}
	for i, raw := range rawContainers {
		container, err := decodeWorkloadContainer(raw, kind, i)
		if err != nil {
			return renderedWorkload{}, err
		}
		workload.containers = append(workload.containers, container)
	}
	return workload, nil
}

// decodeWorkloadVolumes records every hostPath volume's backing host path so
// volumeMounts can be resolved to the mounts they create on the node.
func decodeWorkloadVolumes(w *renderedWorkload, podSpec map[string]any) error {
	rawVolumes, err := optionalSliceField(podSpec, "volumes")
	if err != nil {
		return fmt.Errorf("rendered %s volumes: %w", w.kind, err)
	}
	for i, raw := range rawVolumes {
		volume, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("rendered %s volume %d must be an object", w.kind, i)
		}
		volumeName, err := requiredStringField(volume, "name")
		if err != nil {
			return fmt.Errorf("rendered %s volume %d: %w", w.kind, i, err)
		}
		hostPath, ok := volume["hostPath"].(map[string]any)
		if !ok {
			continue // only hostPath volumes resolve to host mount targets
		}
		hostPathPath, err := requiredStringField(hostPath, "path")
		if err != nil {
			return fmt.Errorf("rendered %s volume %s hostPath: %w", w.kind, volumeName, err)
		}
		w.hostPaths[volumeName] = hostPathPath
	}
	return nil
}

// decodeWorkloadContainer decodes one rendered container's name, args and
// volumeMounts; propagation-relevant mount fields are kept verbatim.
func decodeWorkloadContainer(raw any, kind string, index int) (renderedContainer, error) {
	container, ok := raw.(map[string]any)
	if !ok {
		return renderedContainer{}, fmt.Errorf("rendered %s container %d must be an object", kind, index)
	}
	containerName, err := requiredStringField(container, "name")
	if err != nil {
		return renderedContainer{}, fmt.Errorf("rendered %s container %d: %w", kind, index, err)
	}
	rawArgs, err := optionalSliceField(container, "args")
	if err != nil {
		return renderedContainer{}, fmt.Errorf("rendered %s container %s args: %w", kind, containerName, err)
	}
	current := renderedContainer{name: containerName}
	for _, rawArg := range rawArgs {
		arg, ok := rawArg.(string)
		if !ok {
			return renderedContainer{}, fmt.Errorf(
				"rendered %s container %s args must contain only strings", kind, containerName)
		}
		current.args = append(current.args, arg)
	}
	rawMounts, err := optionalSliceField(container, "volumeMounts")
	if err != nil {
		return renderedContainer{}, fmt.Errorf("rendered %s container %s volumeMounts: %w", kind, containerName, err)
	}
	for i, raw := range rawMounts {
		mount, err := decodeVolumeMount(raw, kind, containerName, i)
		if err != nil {
			return renderedContainer{}, err
		}
		current.mounts = append(current.mounts, mount)
	}
	return current, nil
}

// decodeVolumeMount decodes the propagation-relevant fields of one rendered
// container volumeMount.
func decodeVolumeMount(raw any, kind, containerName string, index int) (renderedMount, error) {
	mount, ok := raw.(map[string]any)
	if !ok {
		return renderedMount{}, fmt.Errorf(
			"rendered %s container %s volumeMount %d must be an object", kind, containerName, index)
	}
	mountName, err := requiredStringField(mount, "name")
	if err != nil {
		return renderedMount{}, fmt.Errorf(
			"rendered %s container %s volumeMount %d: %w", kind, containerName, index, err)
	}
	mountPath, err := requiredStringField(mount, "mountPath")
	if err != nil {
		return renderedMount{}, fmt.Errorf(
			"rendered %s container %s volumeMount %s: %w", kind, containerName, mountName, err)
	}
	entry := renderedMount{volume: mountName, mountPath: mountPath}
	if subPath, ok := mount["subPath"].(string); ok {
		entry.subPath = subPath
	}
	if propagation, ok := mount["mountPropagation"].(string); ok {
		entry.propagation = propagation
	}
	return entry, nil
}

func requiredMapField(parent map[string]any, field string) (map[string]any, error) {
	raw, ok := parent[field]
	if !ok {
		return nil, fmt.Errorf("missing %q", field)
	}
	value, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%q must be an object", field)
	}
	return value, nil
}

func requiredStringField(parent map[string]any, field string) (string, error) {
	raw, ok := parent[field]
	if !ok {
		return "", fmt.Errorf("missing %q", field)
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%q must be a string", field)
	}
	return value, nil
}

func requiredSliceField(parent map[string]any, field string) ([]any, error) {
	raw, ok := parent[field]
	if !ok {
		return nil, fmt.Errorf("missing %q", field)
	}
	value, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%q must be an array", field)
	}
	return value, nil
}

func optionalSliceField(parent map[string]any, field string) ([]any, error) {
	raw, ok := parent[field]
	if !ok {
		return nil, nil
	}
	value, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%q must be an array", field)
	}
	return value, nil
}

func checkFileDriver(r rendered, blockPolicy string) []string {
	const (
		blockName           = "pillar-csi.bhyoo.com"
		fileName            = "files.pillar-csi.bhyoo.com"
		blockNodeSocket     = "/var/lib/kubelet/plugins/pillar-csi.bhyoo.com/csi.sock"
		fileNodeSocket      = "/var/lib/kubelet/plugins/files.pillar-csi.bhyoo.com/csi.sock"
		blockControllerSock = "/csi/csi.sock"
	)
	violations := checkFileDriverResources(r.csiDrivers, blockPolicy, blockName, fileName)
	routes := inspectFileDriverRoutes(r.workloads, fileName,
		blockNodeSocket, fileNodeSocket, blockControllerSock)
	violations = append(violations, fileDriverRouteViolations(routes)...)
	return append(violations, checkFileDriverMounts(r.workloads)...)
}

func checkFileDriverResources(drivers []*storagev1.CSIDriver, blockPolicy, blockName, fileName string) []string {
	byName := make(map[string]*storagev1.CSIDriver, len(drivers))
	for _, driver := range drivers {
		byName[driver.Name] = driver
	}
	var violations []string
	if len(drivers) != 2 {
		violations = append(violations, fmt.Sprintf(
			"fileDriver.enabled must render exactly two CSIDriver resources, got %d", len(drivers)))
	}
	for name, wantPolicy := range map[string]string{blockName: blockPolicy, fileName: "None"} {
		driver, ok := byName[name]
		if !ok {
			violations = append(violations, fmt.Sprintf("fileDriver.enabled must render CSIDriver %s", name))
			continue
		}
		if driver.Spec.FSGroupPolicy == nil || string(*driver.Spec.FSGroupPolicy) != wantPolicy {
			got := "<unset>"
			if driver.Spec.FSGroupPolicy != nil {
				got = string(*driver.Spec.FSGroupPolicy)
			}
			violations = append(violations, fmt.Sprintf(
				"CSIDriver %s fsGroupPolicy=%s, want %s", name, got, wantPolicy))
		}
	}
	return violations
}

type fileDriverRoutes struct {
	blockController bool
	fileController  bool
	fileProvisioner bool
	blockNode       bool
	blockRegistrar  bool
	fileNode        bool
	fileRegistrar   bool
}

func inspectFileDriverRoutes(workloads []renderedWorkload, fileName,
	blockNodeSocket, fileNodeSocket, blockControllerSock string) fileDriverRoutes {
	var routes fileDriverRoutes
	for _, workload := range workloads {
		var workloadRoutes fileDriverRoutes
		for _, container := range workload.containers {
			updateFileDriverRoutes(&workloadRoutes, workload.kind, container, fileName,
				blockNodeSocket, fileNodeSocket, blockControllerSock)
		}
		routes.blockController = routes.blockController || workloadRoutes.blockController
		routes.fileController = routes.fileController || workloadRoutes.fileController
		routes.fileProvisioner = routes.fileProvisioner ||
			workloadRoutes.fileController && workloadRoutes.fileProvisioner
		routes.blockNode = routes.blockNode || workloadRoutes.blockNode
		routes.blockRegistrar = routes.blockRegistrar || workloadRoutes.blockRegistrar
		routes.fileNode = routes.fileNode || workloadRoutes.fileNode
		routes.fileRegistrar = routes.fileRegistrar || workloadRoutes.fileRegistrar
	}
	return routes
}

func updateFileDriverRoutes(routes *fileDriverRoutes, workloadKind string, container renderedContainer,
	fileName, blockNodeSocket, fileNodeSocket, blockControllerSock string) {
	if workloadKind == "Deployment" {
		if hasArgValue(container.args, "--csi-endpoint", "unix://"+blockControllerSock) ||
			hasArgValue(container.args, "--csi-address", blockControllerSock) {
			routes.blockController = true
		}
		if hasArgValue(container.args, "--driver-name", fileName) {
			routes.fileController = true
		}
		if strings.Contains(container.name, "provisioner") &&
			hasNonBlockCSIAddress(container.args, blockControllerSock) {
			routes.fileProvisioner = true
		}
	}
	if workloadKind != "DaemonSet" {
		return
	}
	if hasArgValue(container.args, "--csi-socket", blockNodeSocket) {
		routes.blockNode = true
	}
	if hasArgValue(container.args, "--kubelet-registration-path", blockNodeSocket) {
		routes.blockRegistrar = true
	}
	if hasArgValue(container.args, "--driver-name", fileName) &&
		hasArgValue(container.args, "--csi-socket", fileNodeSocket) {
		routes.fileNode = true
	}
	if strings.Contains(container.name, "registrar") &&
		hasArgValue(container.args, "--kubelet-registration-path", fileNodeSocket) {
		routes.fileRegistrar = true
	}
}

func fileDriverRouteViolations(routes fileDriverRoutes) []string {
	var violations []string
	if !routes.blockController {
		violations = append(violations, "fileDriver.enabled must preserve the block controller socket route")
	}
	if !routes.fileController {
		violations = append(violations,
			"fileDriver.enabled must render a controller with --driver-name=files.pillar-csi.bhyoo.com")
	}
	if !routes.fileProvisioner {
		violations = append(violations,
			"file-driver provisioner must use a non-block --csi-address in the file controller workload")
	}
	if !routes.blockNode || !routes.blockRegistrar {
		violations = append(violations, "fileDriver.enabled must preserve the block node socket and registrar routes")
	}
	if !routes.fileNode {
		violations = append(violations, "fileDriver.enabled must pair the file node --driver-name with its file socket")
	}
	if !routes.fileRegistrar {
		violations = append(violations, "fileDriver.enabled must render a file registrar --kubelet-registration-path")
	}
	return violations
}

// checkFileDriverMounts enforces the agent's mount-propagation contract:
// no two Bidirectional mounts in one container may stack on equal or nested
// host paths or mount paths (the agent VerifyMount rejects stacked mountinfo
// records, and stacked propagation loses the nested mount's lifecycle), and
// the container that owns --nfs-export-root must see that host path through
// exactly one Bidirectional mount — either a dedicated hostPath equal to the
// root, or an enclosing mount (agent-state) that covers it by the same
// relative path on both sides.
func checkFileDriverMounts(workloads []renderedWorkload) []string {
	var violations []string
	for _, workload := range workloads {
		for _, container := range workload.containers {
			violations = append(violations, containerFileMountViolations(workload, container)...)
		}
	}
	return violations
}

// containerFileMountViolations applies both mount-propagation invariants to
// one container's mounts.
func containerFileMountViolations(workload renderedWorkload, container renderedContainer) []string {
	var bidirectional []renderedMount
	for _, mount := range container.mounts {
		if mount.propagation == "Bidirectional" {
			bidirectional = append(bidirectional, mount)
		}
	}
	violations := bidirectionalOverlapViolations(workload, container, bidirectional)
	return append(violations, exportRootCoverageViolations(workload, container, bidirectional)...)
}

// bidirectionalOverlapViolations reports every pair of one container's
// Bidirectional mounts that stacks on equal or nested mount paths or backing
// host paths — two mounts propagating events on the same tree.
func bidirectionalOverlapViolations(workload renderedWorkload, container renderedContainer,
	bidirectional []renderedMount) []string {
	var violations []string
	for i, left := range bidirectional {
		for _, right := range bidirectional[i+1:] {
			leftHost := workload.mountHostPath(left)
			rightHost := workload.mountHostPath(right)
			if relatedPaths(left.mountPath, right.mountPath) ||
				leftHost != "" && rightHost != "" && relatedPaths(leftHost, rightHost) {
				violations = append(violations, fmt.Sprintf(
					"%s %s container %s stacks Bidirectional mounts %s and %s on equal or nested paths",
					workload.kind, workload.name, container.name, left.mountPath, right.mountPath))
			}
		}
	}
	return violations
}

// exportRootCoverageViolations requires the container owning
// --nfs-export-root to see that host path through exactly one Bidirectional
// mount: a dedicated hostPath equal to the root, or an enclosing mount such
// as agent-state covering it by the same relative path on both sides.
func exportRootCoverageViolations(workload renderedWorkload, container renderedContainer,
	bidirectional []renderedMount) []string {
	exportRoot := argValue(container.args, "--nfs-export-root")
	if exportRoot == "" {
		return nil
	}
	covers := 0
	for _, mount := range bidirectional {
		if mountCoversHostPath(mount, workload.mountHostPath(mount), exportRoot) {
			covers++
		}
	}
	if covers != 1 {
		return []string{fmt.Sprintf(
			"%s %s container %s must cover --nfs-export-root %s with exactly one Bidirectional mount, got %d",
			workload.kind, workload.name, container.name, exportRoot, covers)}
	}
	return nil
}

// mountHostPath resolves a volumeMount to its effective host path: the
// backing hostPath.path plus any subPath offset. Non-hostPath volumes have no
// host mount target and resolve to "".
func (w renderedWorkload) mountHostPath(mount renderedMount) string {
	hostPath := w.hostPaths[mount.volume]
	if hostPath == "" {
		return ""
	}
	if mount.subPath != "" {
		return filepath.Join(hostPath, mount.subPath)
	}
	return hostPath
}

// childRel returns path's relative path beneath base, "." for equality, or
// ".." when path is not beneath base (also when either path is empty).
func childRel(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ".."
	}
	return rel
}

// relatedPaths reports whether two paths are equal or one is a parent of the
// other — the stacked-mount relation, independent of mount order.
func relatedPaths(a, b string) bool {
	return childRel(a, b) != ".." || childRel(b, a) != ".."
}

// mountCoversHostPath reports whether a mount exposes host path root: root
// must sit at the same relative path beneath both the mountPath and the
// backing host path (equal paths included, e.g. an exact file-proxy-root or a
// proxy root nested under agent-state).
func mountCoversHostPath(mount renderedMount, hostPath, root string) bool {
	fromMount := childRel(mount.mountPath, root)
	return fromMount != "" && fromMount != ".." && fromMount == childRel(hostPath, root)
}

// argValue returns the value of a --flag=value or --flag value pair, "" when
// the flag is absent.
func argValue(args []string, flagName string) string {
	for i, arg := range args {
		if arg == flagName && i+1 < len(args) {
			return args[i+1]
		}
		if value, ok := strings.CutPrefix(arg, flagName+"="); ok {
			return value
		}
	}
	return ""
}

func hasArgValue(args []string, flagName, want string) bool {
	for i, arg := range args {
		if arg == flagName && i+1 < len(args) && args[i+1] == want {
			return true
		}
		if strings.HasPrefix(arg, flagName+"=") && strings.TrimPrefix(arg, flagName+"=") == want {
			return true
		}
	}
	return false
}

func hasNonBlockCSIAddress(args []string, blockSocket string) bool {
	for i, arg := range args {
		value := ""
		switch {
		case arg == "--csi-address" && i+1 < len(args):
			value = args[i+1]
		case strings.HasPrefix(arg, "--csi-address="):
			value = strings.TrimPrefix(arg, "--csi-address=")
		}
		if value != "" && value != blockSocket {
			return true
		}
	}
	return false
}

type crdSet = map[string]*apiextv1.CustomResourceDefinition

func checkCRDs(gen crdSet, got []*apiextv1.CustomResourceDefinition, expectNone bool) []string {
	var v []string
	if expectNone {
		for _, crd := range got {
			v = append(v, fmt.Sprintf("installCRDs=false still renders CRD %s", crd.Name))
		}
		return v
	}
	seen := make(map[string]bool, len(got))
	for _, crd := range got {
		seen[crd.Name] = true
		v = append(v, checkCRD(gen[crd.Name], crd)...)
	}
	for name := range gen {
		if !seen[name] {
			v = append(v, fmt.Sprintf("controller-gen CRD %s is not rendered by the chart", name))
		}
	}
	return v
}

func checkCRD(want, got *apiextv1.CustomResourceDefinition) []string {
	var v []string
	switch {
	case want == nil:
		v = append(v, fmt.Sprintf("chart CRD %s has no controller-gen source", got.Name))
	case !equality.Semantic.DeepEqual(want.Spec, got.Spec):
		v = append(v, fmt.Sprintf("chart CRD %s spec differs from controller-gen", got.Name))
	}
	for _, s := range got.Spec.Names.ShortNames {
		if builtinShortNames[s] {
			v = append(v, fmt.Sprintf("chart CRD %s shortName %q shadows a built-in resource", got.Name, s))
		}
	}
	return v
}

func checkRBAC(gen crdSet, genRole *rbacv1.ClusterRole, roles []*rbacv1.ClusterRole, controllerRole string) []string {
	served := make(map[string]bool, len(gen))
	for _, crd := range gen {
		served[crd.Spec.Names.Plural] = true
	}
	var v []string
	var controller *rbacv1.ClusterRole
	for _, role := range roles {
		if role.Name == controllerRole {
			controller = role
		}
		v = append(v, unservedGrants(role, served)...)
	}
	if controller == nil {
		return append(v, fmt.Sprintf("controller ClusterRole %q is not rendered", controllerRole))
	}
	return append(v, missingGrants(controller, genRole)...)
}

// unservedGrants reports pillar-csi.bhyoo.com resources a role grants that no
// controller-gen CRD serves; RESTMapper-based clients never request them.
func unservedGrants(role *rbacv1.ClusterRole, served map[string]bool) []string {
	var v []string
	for _, rule := range role.Rules {
		if !slices.Contains(rule.APIGroups, pillarGroup) {
			continue
		}
		for _, res := range rule.Resources {
			if !served[strings.SplitN(res, "/", 2)[0]] {
				v = append(v, fmt.Sprintf("role %s grants %s/%s, which no controller-gen CRD serves",
					role.Name, pillarGroup, res))
			}
		}
	}
	return v
}

// missingGrants reports every generated (group, resource, verb) permission the
// chart controller role does not allow.
func missingGrants(controller, genRole *rbacv1.ClusterRole) []string {
	var v []string
	for _, rule := range genRole.Rules {
		for _, perm := range expand(rule) {
			if !allows(controller.Rules, perm) {
				v = append(v, fmt.Sprintf("controller ClusterRole %s lacks generated permission %s %q/%s",
					controller.Name, perm.verb, perm.group, perm.resource))
			}
		}
	}
	return v
}

type permission struct{ group, resource, verb string }

func expand(rule rbacv1.PolicyRule) []permission {
	perms := make([]permission, 0, len(rule.APIGroups)*len(rule.Resources)*len(rule.Verbs))
	for _, g := range rule.APIGroups {
		for _, r := range rule.Resources {
			for _, verb := range rule.Verbs {
				perms = append(perms, permission{group: g, resource: r, verb: verb})
			}
		}
	}
	return perms
}

func allows(rules []rbacv1.PolicyRule, p permission) bool {
	for _, r := range rules {
		verbOK := slices.Contains(r.Verbs, p.verb) || slices.Contains(r.Verbs, rbacv1.VerbAll)
		if verbOK && slices.Contains(r.APIGroups, p.group) && slices.Contains(r.Resources, p.resource) {
			return true
		}
	}
	return false
}
