//go:build e2e
// +build e2e

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

package e2e

import (
	"embed"
)

// TestdataFS holds all YAML files under test/e2e/testdata/ as an embedded
// filesystem.  Use it when a helper needs to write a YAML to a temp file
// (e.g. "kind create cluster --config <file>") or iterate over multiple
// values files.
//
//go:embed testdata
var TestdataFS embed.FS

// KindConfigYAML is the Kind cluster configuration used to create the e2e
// test cluster.  It defines 1 control-plane node and 2 worker nodes, with
// the storage worker node labelled pillar-csi.bhyoo.com/storage-node=true
// and /sys/kernel/config mounted for NVMe-oF target module access.
//
//go:embed testdata/kind-config.yaml
var KindConfigYAML []byte

// HelmValuesYAML contains the base Helm values applied when installing
// pillar-csi in internal-agent mode during e2e BeforeSuite.  Images are
// pre-tagged "e2e" and pull policy is Never so that locally-loaded Kind
// images are used without any registry access.
//
//go:embed testdata/helm-values.yaml
var HelmValuesYAML []byte

// HelmValuesExternalYAML contains the Helm values overlay applied on top of
// HelmValuesYAML when running the external-agent test suite.  It disables
// the in-cluster agent DaemonSet by using an unmatchable nodeSelector,
// allowing the test framework to start an out-of-cluster agent container
// instead.
//
//go:embed testdata/helm-values-external.yaml
var HelmValuesExternalYAML []byte
