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
//   - a controller-gen RBAC permission missing from the chart controller role.
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
	basesDir       string
	rolePath       string
	controllerRole string
	expectNoCRDs   bool
}

type rendered struct {
	crds  []*apiextv1.CustomResourceDefinition
	roles []*rbacv1.ClusterRole // Roles are decoded into ClusterRole: identical rules shape
}

func main() {
	var o options
	flag.StringVar(&o.basesDir, "bases", "config/crd/bases", "controller-gen CRD directory")
	flag.StringVar(&o.rolePath, "role", "config/rbac/role.yaml", "controller-gen ClusterRole")
	flag.StringVar(&o.controllerRole, "controller-role", "", "name of the rendered controller ClusterRole")
	flag.BoolVar(&o.expectNoCRDs, "expect-no-crds", false, "the render used installCRDs=false")
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
		return nil, err
	}
	genRole := &rbacv1.ClusterRole{}
	err = decodeFile(o.rolePath, genRole)
	if err != nil {
		return nil, err
	}
	r, err := decodeRender(render)
	if err != nil {
		return nil, err
	}

	v := checkCRDs(gen, r.crds, o.expectNoCRDs)
	v = append(v, checkRBAC(gen, genRole, r.roles, o.controllerRole)...)
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
			return nil, decodeErr
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
		switch meta.Kind {
		case "CustomResourceDefinition":
			crd := &apiextv1.CustomResourceDefinition{}
			err = yaml.UnmarshalStrict(doc, crd)
			if err != nil {
				return r, fmt.Errorf("decode rendered CRD: %w", err)
			}
			r.crds = append(r.crds, crd)
		case "ClusterRole", "Role":
			role := &rbacv1.ClusterRole{}
			err = yaml.Unmarshal(doc, role)
			if err != nil {
				return r, fmt.Errorf("decode rendered %s: %w", meta.Kind, err)
			}
			r.roles = append(r.roles, role)
		}
	}
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
