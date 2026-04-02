// Package registry — Resource interface.
//
// This file defines the [Resource] interface that every ephemeral storage
// backend must implement to participate in the Registry lifecycle.  New backend
// types become first-class registry citizens simply by satisfying this
// interface and calling [Registry.Register] — no framework code changes are
// required.
package registry

import "context"

// Resource is the extensibility contract for ephemeral storage backends.
//
// Any type that satisfies Resource can be registered with [Registry.Register]
// without any changes to the registry framework itself — this is the primary
// extensibility point for new backend types (ZFS, LVM, iSCSI, NVMe-oF, …).
//
// # Implementing Resource
//
// A backend type satisfies Resource by providing two methods:
//
//  1. Destroy — tears down the ephemeral storage resource and releases all
//     associated host or container resources.  Destroy MUST be idempotent:
//     calling it on an already-destroyed resource must return nil, not an error.
//     Calling Destroy on a nil receiver must also be a safe no-op (return nil).
//
//  2. Description — returns a human-readable identity string used in error
//     messages and diagnostic logs.
//
// # Example: adding a hypothetical NVMe namespace backend
//
// No registry code changes are needed — just implement the two methods:
//
//	// In package nvme:
//	type Namespace struct {
//	    NodeContainer string
//	    NSID          int
//	}
//
//	func (n *Namespace) Destroy(ctx context.Context) error { … }
//	func (n *Namespace) Description() string {
//	    return fmt.Sprintf("nvme namespace %d on container %q", n.NSID, n.NodeContainer)
//	}
//
//	// In the test — no registry changes required:
//	ns, err := nvme.CreateNamespace(ctx, nvme.CreateNamespaceOptions{…})
//	reg.Register(ns)
//
// # Built-in implementations
//
//   - [zfs.Pool]    — ephemeral ZFS pool on a loop device
//   - [lvm.VG]      — ephemeral LVM Volume Group on a loop device
//   - [iscsi.Target] — ephemeral iSCSI target backed by a loop device
type Resource interface {
	// Destroy releases all storage resources associated with this ephemeral
	// backend instance.  Must be idempotent and nil-safe.
	Destroy(ctx context.Context) error

	// Description returns a human-readable identifier for this resource,
	// used in error messages (e.g. `zfs pool "tank" on container "worker"`).
	Description() string
}
