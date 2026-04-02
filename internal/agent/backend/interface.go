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

package backend

import "context"

// BackendProvisioner is the extensibility contract for storage backend
// lifecycle management in the production agent.
//
// It complements [VolumeBackend] (which handles per-volume operations such as
// Create, Delete, and Expand) by covering pool-level or volume-group-level
// lifecycle operations that span the entire backend, not individual volumes.
//
// # Extension model
//
// Adding a new backend type (e.g. Ceph RBD, NVMe-oF subsystem) does NOT
// require modifying any agent core code.  The workflow is:
//
//  1. Define a type that implements BackendProvisioner.
//  2. Call the package-level [Register] function with a type name and an
//     instance of that type — typically from an init() function or the agent's
//     startup sequence.
//  3. The agent core iterates [Registered] / calls [Lookup] without knowing
//     the concrete type.
//
// # Lifecycle
//
// The three method phases map to the agent startup/shutdown sequence:
//
//	Register (global fn) → Register (method) → Provision → (agent serves RPCs) → Teardown
//
// [Register] (method) is called once to enroll the backend before any storage
// resources are created. [Provision] creates or activates the storage
// resource. [Teardown] deactivates and, optionally, destroys it on shutdown.
//
// # Idempotency
//
// [Provision] and [Teardown] MUST be idempotent: re-calling them when the
// resource is already in the target state must not return an error.
//
//nolint:revive // stutter intentional: backend.BackendProvisioner is clearer at call sites than backend.Provisioner
type BackendProvisioner interface {
	// Register enrolls this backend with the agent runtime. It is called once
	// during startup, before Provision, and may be used to validate
	// configuration, pre-flight-check kernel module presence, or allocate
	// internal bookkeeping structures.
	//
	// Register must not create persistent storage resources; that is the
	// responsibility of Provision.
	//
	// Returning a non-nil error from Register causes the agent to treat this
	// backend as unavailable.
	Register(ctx context.Context) error

	// Provision creates or activates the backend's storage resource.
	//
	// Examples:
	//   - ZFS:  "zpool create <pool> <device>" or "zpool import <pool>"
	//   - LVM:  "pvcreate <dev>" + "vgcreate <vg> <dev>" + "vgchange -ay <vg>"
	//
	// Provision is called once after Register succeeds. On success the backend
	// is considered "active" and the agent may service VolumeBackend RPCs.
	//
	// Idempotent: if the resource already exists and is active, Provision MUST
	// return nil without creating a duplicate resource.
	Provision(ctx context.Context) error

	// Teardown deactivates and, if configured to do so, destroys the backend's
	// storage resource. It is called once during agent shutdown, after all
	// in-flight VolumeBackend operations have drained.
	//
	// Examples:
	//   - ZFS:  "zpool export <pool>" or "zpool destroy -f <pool>"
	//   - LVM:  "vgchange -an <vg>"
	//
	// Idempotent: if the resource is already inactive or absent, Teardown MUST
	// return nil.
	Teardown(ctx context.Context) error
}

// ─── Global registry ─────────────────────────────────────────────────────────.

// provisioners is the global registry of BackendProvisioner implementations,
// keyed by the backend type name (e.g. "zfs", "lvm", "ceph-rbd").
//
// Access is intentionally not guarded by a mutex: Register is expected to be
// called only during process startup (init() functions or main), before any
// concurrent goroutines are spawned.
var provisioners = make(map[string]BackendProvisioner)

// Register adds bp to the global BackendProvisioner registry under backendType.
//
// If two BackendProvisioner implementations are registered under the same
// backendType, the last call wins. Passing a nil bp or an empty backendType is
// a safe no-op.
//
// Register is the primary entry point for plugging new backends into the agent
// without modifying any existing code — see the [BackendProvisioner] doc
// comment for the full extension recipe.
//
// The backendType string should be a short, lowercase identifier, e.g.:
//
//	backend.Register("zfs",      &zfs.Provisioner{PoolName: "tank"})
//	backend.Register("lvm",      &lvm.Provisioner{VGName:   "vg0"})
//	backend.Register("ceph-rbd", &ceph.Provisioner{...})
func Register(backendType string, bp BackendProvisioner) {
	if bp == nil || backendType == "" {
		return
	}
	provisioners[backendType] = bp
}

// Lookup returns the BackendProvisioner registered under backendType, or
// (nil, false) when no implementation has been registered for that type.
func Lookup(backendType string) (BackendProvisioner, bool) {
	p, ok := provisioners[backendType]
	return p, ok
}

// Registered returns the names of all currently-registered backend types in
// unspecified order. It is safe to call at any point after all Register calls
// have returned.
func Registered() []string {
	names := make([]string, 0, len(provisioners))
	for name := range provisioners {
		names = append(names, name)
	}
	return names
}
