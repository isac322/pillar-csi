# ADR-001: Reconciler Design for Pillar-CSI CRD Controllers

**Date:** 2026-03-23
**Status:** Accepted

---

## Context

Pillar-CSI manages storage provisioning across four interdependent CRDs:
`PillarTarget`, `PillarPool`, `PillarProtocol`, and `PillarBinding`.  Each
resource has both lifecycle dependencies (a Binding needs a Pool and a Protocol;
a Pool needs a Target) and cleanup dependencies (a Target must not be deleted
while Pools still reference it).

Without explicit reconcile logic these relationships would be invisible to
Kubernetes and operators would face silent data-loss scenarios: e.g. a Target
deleted under an active Pool, or a StorageClass orphaned after a Binding is
removed while PVCs still hold volumes.

---

## Decisions and Rationale

### 1. Finalizer-based deletion protection on every CRD

**Why:** Kubernetes `ownerReference` cascade-delete cannot protect across
namespace boundaries or in cases where the "owner" is the *dependent* resource
rather than the parent (e.g. a PillarPool is not owned by a PillarTarget; it
merely references one).  Finalizers give the controller an explicit hook to
block the DELETE call until all safety checks pass, producing an observable,
retryable failure instead of a silent race condition.

Each finalizer is domain-scoped (`pillar-csi.bhyoo.com/*-protection`) so that
the controller that added the finalizer is unambiguous.  The finalizer is added
on the *first* reconcile (creation) and removed only after all back-references
have been cleared.

### 2. Status conditions over simple boolean fields

**Why:** Kubernetes operators and monitoring pipelines depend on structured
machine-readable status.  A plain `ready: false` field gives no context for
automated alerting or human debugging; a condition like
`Ready=False/PoolNotFound` is self-documenting.  Using the standard
`metav1.Condition` type (with `Type`, `Status`, `Reason`, `Message`, and
`LastTransitionTime`) enables standard tooling (e.g. `kubectl wait
--for=condition=Ready`) and prevents operators from having to parse log lines.

### 3. ownerReference on the generated StorageClass

**Why:** A `PillarBinding` drives the creation of exactly one `StorageClass`.
The StorageClass must be garbage-collected when the Binding is deleted, but
`StorageClass` is a cluster-scoped resource while `PillarBinding` may be
namespace-scoped.  `SetControllerReference` from controller-runtime handles the
cross-scope bookkeeping and makes the ownership explicit in `kubectl describe
storageclass`.  The Binding additionally blocks deletion while PVCs still use
the class, preventing silent data unavailability.

### 4. Node auto-labeling from PillarTarget reconcile

**Why:** The CSI node plugin needs to advertise which nodes can serve as storage
targets without requiring a separate DaemonSet or manual label management.
Driving the label (`pillar-csi.bhyoo.com/storage-node=true`) from the
`PillarTarget` reconciler means the label lifecycle is tied to the existence of
a valid, connected target, not to operator intervention.  The label is removed
during finalizer cleanup so workloads are not scheduled to a node whose storage
target has been decommissioned.

### 5. Requeue strategy: watch events first, timed fallback

**Why:** Pure timed requeue (every N seconds) wastes API server calls when
nothing has changed.  Pure watch-only reconcile can stall if a watch event is
missed (e.g. transient network error).  The controllers use
`SetupWithManager` secondary watches to re-enqueue the resource promptly when
its dependencies change, combined with a short `RequeueAfter` (10 s for
deletion-blocked states, 15 s for not-ready states) as a safety net.  This
keeps reconcile latency low without hammering the API server.

### 6. gRPC agent calls are stubbed in this iteration

**Why:** The agent gRPC surface (`AgentConnected`, `PoolDiscovered`,
`BackendSupported`, `Capabilities`) depends on a separate in-cluster agent
process that is out of scope for this task.  Stubbing the conditions with
`Unknown/AgentConnectionNotImplemented` makes the current state explicit to
operators and keeps the controller testable in envtest without requiring a live
agent.  The integration point is clearly marked in code comments for Task 3.

---

## Consequences

- All four controllers are independently reconcilable and observable via
  `kubectl get/describe` on their respective CRDs.
- No resource can be force-deleted by a simple `kubectl delete` while
  back-references exist — operators must clean up dependents first.
- The StorageClass is fully managed by Kubernetes GC once its owning Binding
  is unblocked and deleted.
- Node labels are authoritative: their presence implies an active, validated
  target; their absence means the node should not receive storage workloads.
- The test suite (147 specs, envtest) exercises all condition transitions,
  requeue paths, and deletion-protection scenarios without a live cluster.
