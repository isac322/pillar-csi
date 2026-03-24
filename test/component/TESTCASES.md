# Component Test Case Specification

This document is the **authoritative spec** for pillar-csi component tests.
Component tests treat each major subsystem as a **unit-of-behavior**, wiring mock
dependencies at the subsystem boundary and testing feature-level behavior
including all exception paths.

**Rules:**
- Every test function must trace back to an entry in this document.
- Every entry must have a corresponding test function implementation.
- Cross-cutting failure modes (permission denied, timeout, TOCTOU, concurrent
  ops, partial failure) appear as test cases **within the component they
  exercise**, not in a separate pseudo-component.

**Total test cases: 271**

---

## Mock Fidelity Notes

### mockVolumeBackend (used by Agent gRPC tests)
```go
// mockVolumeBackend is a field-based test double for backend.VolumeBackend.
// Simplifications vs. real ZFS backend:
//   - No command execution; results are preset fields.
//   - Mutex protects concurrent reads but there is no retry or partial-write logic.
//   - ConflictError is returned by setting createErr directly; no size-check logic.
//   - DevicePath is a simple string field; no filesystem stat.
```

### seqExec (used by ZFS Backend tests)
```go
// seqExec replays preset execResponse values in strict order.
// Simplifications vs. real os/exec:
//   - No actual process spawning; no real stdout/stderr pipes.
//   - Does not honour context.Done() unless the test explicitly blocks.
//   - Call ordering is strict: unexpected extra calls fatalf the test.
//   - Exit codes are simulated via errors.New("exit status 1").
```

### NvmetTarget with t.TempDir() (used by NVMe-oF tests)
```go
// NvmetTarget.ConfigfsRoot is set to t.TempDir() (a real tmpfs directory).
// Simplifications vs. real /sys/kernel/config:
//   - No kernel nvmet module; writes are normal filesystem writes.
//   - "enable" files can be opened for write (real configfs triggers NVMe target).
//   - Symlinks work normally (real configfs symlinks trigger kernel port-binding).
//   - Read-only tests use chmod; behaviour matches real permission denial.
```

### mockAgentClient (used by CSI tests)
```go
// mockAgentClient implements agentv1.AgentServiceClient with function fields.
// Simplifications vs. real gRPC client:
//   - No network I/O; calls are in-process.
//   - No retry or connection-pool logic.
//   - gRPC status codes are returned directly; no transport-layer wrapping.
//   - Streaming RPCs are not exercised.
```

### mockIdentityReadyFn (used by CSI Identity tests)
```go
// mockIdentityReadyFn is a preset test double for the IdentityServer readyFn.
// Simplifications vs. a real health check:
//   - No I/O; result is controlled by test-supplied fields.
//   - Blocking variant uses ctx.Done() to simulate a slow check that
//     respects context cancellation; real checks would access kernel/network.
//   - Does not validate subsystem state (kernel modules, agent reachability).
```

---

## Component 1: Agent gRPC Server (`internal/agent/`)

**Subsystem-boundary setup:** `agent.Server` constructed with a mock
`backend.VolumeBackend` and real configfs (`t.TempDir()`).  Handler methods are
called directly (no network).  Cross-cutting failure paths (context cancellation,
TOCTOU, concurrency) are tested here because they exercise the agent server's
error-handling logic.

---

### 1.1 Volume Lifecycle: CreateVolume

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 1 | `TestAgentServer_CreateVolume_Success` | Normal volume creation returns device path and allocated size | Mock backend: devicePath="/dev/zvol/tank/pvc-abc", allocatedBytes=10 GiB | Returns CreateVolumeResponse with device_path and capacity_bytes; no error |
| 2 | `TestAgentServer_CreateVolume_Idempotent` | Creating same volume twice returns identical result | Mock backend always returns same device path; request issued twice | Both calls succeed; no error |
| 3 | `TestAgentServer_CreateVolume_DiskFull` | Disk-full backend error maps to non-OK gRPC status | Mock backend: createErr="out of space" | Returns non-OK gRPC status; code ≠ OK |
| 4 | `TestAgentServer_CreateVolume_InvalidPool` | Volume ID referencing unknown pool returns NotFound | VolumeID="missing-pool/pvc-xyz"; server has no backend for that pool | Returns gRPC NotFound |
| 5 | `TestAgentServer_CreateVolume_InvalidVolumeID` | Malformed volume ID (no slash) returns InvalidArgument | VolumeID="noslash" | Returns gRPC InvalidArgument |
| 6 | `TestAgentServer_CreateVolume_BackendError` | Generic backend error maps to Internal | Mock backend: createErr="unexpected ZFS failure" | Returns gRPC Internal |
| 7 | `TestAgentServer_CreateVolume_ConflictSize` | Existing volume with different capacity returns AlreadyExists | Mock backend: createErr=ConflictError{existing=10GiB, requested=20GiB} | Returns gRPC AlreadyExists |

---

### 1.2 Volume Lifecycle: DeleteVolume

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 8 | `TestAgentServer_DeleteVolume_Success` | Normal deletion returns empty response | Mock backend: deleteErr=nil | Returns DeleteVolumeResponse; no error |
| 9 | `TestAgentServer_DeleteVolume_Idempotent` | Deleting non-existent volume (backend returns nil) succeeds | Mock backend always returns nil on Delete | Returns empty response; no error |
| 10 | `TestAgentServer_DeleteVolume_InvalidPool` | Unknown pool returns NotFound | VolumeID="other-pool/pvc-abc"; no backend registered for that pool | Returns gRPC NotFound |
| 11 | `TestAgentServer_DeleteVolume_DeviceBusy` | Device busy error maps to Internal | Mock backend: deleteErr="dataset is busy" | Returns gRPC Internal |
| 12 | `TestAgentServer_DeleteVolume_BackendError` | Generic delete error maps to Internal | Mock backend: deleteErr="unexpected failure" | Returns gRPC Internal |

---

### 1.3 Volume Lifecycle: ExpandVolume

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 13 | `TestAgentServer_ExpandVolume_Success` | Normal expansion returns new capacity | Mock backend: expandAllocated=20 GiB | Returns ExpandVolumeResponse with capacity_bytes=20 GiB; no error |
| 14 | `TestAgentServer_ExpandVolume_ShrinkRejected` | Shrink attempt at backend level propagates as Internal | Mock backend: expandErr="volsize cannot be decreased" | Returns gRPC Internal |
| 15 | `TestAgentServer_ExpandVolume_NotFound` | Expanding non-existent volume returns error | Mock backend: expandErr="dataset does not exist" | Returns non-OK gRPC status |
| 16 | `TestAgentServer_ExpandVolume_InvalidPool` | Unknown pool returns NotFound | VolumeID="other-pool/pvc-abc" | Returns gRPC NotFound |

---

### 1.4 Export Lifecycle: ExportVolume

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 17 | `TestAgentServer_ExportVolume_Success` | Full NVMe-oF export creates configfs entries and returns ExportInfo | Mock backend: devicePath="/dev/zvol/tank/pvc-abc"; real tmpdir configfs | Returns ExportInfo with target_id (NQN), address, port; subsystem dir created in tmpdir |
| 18 | `TestAgentServer_ExportVolume_Idempotent` | Re-exporting same volume is a no-op | Export called twice with identical params | Both succeed; same ExportInfo returned; no error |
| 19 | `TestAgentServer_ExportVolume_InvalidProtocol` | Unsupported protocol type returns Unimplemented | protocol_type=PROTOCOL_TYPE_ISCSI | Returns gRPC Unimplemented |
| 20 | `TestAgentServer_ExportVolume_MissingParams` | Missing NVMe-oF params return InvalidArgument | protocol_type=NVMEOF_TCP, no ExportParams field | Returns gRPC InvalidArgument |
| 21 | `TestAgentServer_ExportVolume_DeviceNotReady` | Device never appears within poll window returns FailedPrecondition | DeviceChecker always returns false; poll timeout=20 ms | Returns gRPC FailedPrecondition |
| 22 | `TestAgentServer_ExportVolume_DeviceAppearsAfterDelay` | Device appears after several poll retries; export succeeds | DeviceChecker returns false twice, then true on 3rd call | Returns ExportInfo successfully; configfs dir created |
| 23 | `TestAgentServer_ExportVolume_PermissionError` | Permission denied on device check propagates as FailedPrecondition | DeviceChecker returns os.ErrPermission | Returns gRPC FailedPrecondition |

---

### 1.5 Export Lifecycle: UnexportVolume

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 24 | `TestAgentServer_UnexportVolume_Success` | Unexport removes configfs entries | Volume exported first; then unexported | Returns empty response; configfs subsystem dir removed |
| 25 | `TestAgentServer_UnexportVolume_Idempotent` | Unexporting non-exported volume is a no-op | No prior export | Returns empty response; no error |
| 26 | `TestAgentServer_UnexportVolume_InvalidProtocol` | Unsupported protocol type returns error | protocol_type=PROTOCOL_TYPE_ISCSI | Returns non-OK gRPC status |

---

### 1.6 ACL Management

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 27 | `TestAgentServer_AllowInitiator_Success` | Granting access creates ACL symlink in configfs | Volume exported; AllowInitiator called with host NQN | Returns empty response; symlink created in allowed_hosts/ |
| 28 | `TestAgentServer_AllowInitiator_Idempotent` | Allowing same initiator twice is a no-op | AllowInitiator called twice with same host NQN | Both succeed; single symlink; no error |
| 29 | `TestAgentServer_AllowInitiator_InvalidProtocol` | Invalid protocol for AllowInitiator returns error | protocol_type=PROTOCOL_TYPE_ISCSI | Returns non-OK gRPC status |
| 30 | `TestAgentServer_DenyInitiator_Success` | Denying access removes ACL symlink | Volume exported; initiator allowed; DenyInitiator called | Returns empty response; symlink removed from allowed_hosts/ |
| 31 | `TestAgentServer_DenyInitiator_Idempotent` | Denying already-denied initiator is a no-op | No prior AllowInitiator | Returns empty response; no error |
| 32 | `TestAgentServer_DenyInitiator_InvalidProtocol` | Invalid protocol for DenyInitiator returns error | protocol_type=PROTOCOL_TYPE_ISCSI | Returns non-OK gRPC status |

---

### 1.7 Discovery: GetCapabilities / GetCapacity / ListVolumes / ListExports

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 33 | `TestAgentServer_GetCapabilities_ReturnsAll` | Returns all supported capabilities including ZFS backend and NVMe-oF | Server configured with ZFS backend + NVMe-oF protocol | Response includes ZFS_ZVOL backend type and NVMEOF_TCP protocol |
| 34 | `TestAgentServer_GetCapacity_Success` | Returns pool capacity stats | Mock backend: total=100 GiB, available=60 GiB | Response with total_bytes=100 GiB, available_bytes=60 GiB |
| 35 | `TestAgentServer_GetCapacity_PoolOffline` | Pool offline maps to non-OK status | Mock backend: capacityErr="pool is not available" | Returns non-OK gRPC status |
| 36 | `TestAgentServer_GetCapacity_UnknownPool` | Unknown pool returns NotFound | Request for pool that has no registered backend | Returns gRPC NotFound |
| 37 | `TestAgentServer_ListVolumes_Success` | Lists all volumes returned by backend | Mock backend: 3 VolumeInfo entries | Response contains exactly 3 volumes with correct fields |
| 38 | `TestAgentServer_ListVolumes_Empty` | Empty pool returns empty list | Mock backend: empty slice | Response contains 0 volumes; no error |
| 39 | `TestAgentServer_ListVolumes_BackendError` | Backend list error propagates | Mock backend: listErr="zfs error" | Returns non-OK gRPC status |
| 40 | `TestAgentServer_ListExports_ReturnsEmpty` | Clean configfs returns empty export list | Fresh tmpdir configfs | Response contains 0 entries; no error |

---

### 1.8 Health Check

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 41 | `TestAgentServer_HealthCheck_AllHealthy` | All subsystems report healthy | ZFS module path points to existing tmp file; pool not degraded | Returns HEALTHY for all subsystem checks |
| 42 | `TestAgentServer_HealthCheck_ZFSModuleMissing` | ZFS kernel module not loaded returns UNHEALTHY | sysModuleZFSPath points to non-existent path | Returns UNHEALTHY for ZFS module check |
| 43 | `TestAgentServer_HealthCheck_ConfigfsMissing` | nvmet configfs not mounted returns degraded | configfsRoot points to non-existent path | Returns UNHEALTHY or DEGRADED for configfs check |
| 44 | `TestAgentServer_HealthCheck_PoolDegraded` | Pool degraded state is surfaced | Mock backend health returns degraded state | Returns DEGRADED for pool health check |

---

### 1.9 Recovery: ReconcileState

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 45 | `TestAgentServer_ReconcileState_ReExportsAfterRestart` | Post-restart reconcile re-creates configfs entries | ReconcileState called with list of volumes to export; clean tmpdir configfs | configfs subsystem dirs created for all volumes in the list |
| 46 | `TestAgentServer_ReconcileState_Idempotent` | ReconcileState is safe to call multiple times | Called twice with same volume list | Same final configfs state on both calls; no error |
| 47 | `TestAgentServer_ReconcileState_MultipleVolumes` | Multiple volumes are all reconciled | ReconcileState with 3 volumes | All 3 configfs subsystem dirs created; no error |
| 48 | `TestAgentServer_ReconcileState_EmptyList` | Empty reconcile list is handled gracefully | ReconcileState with empty list | No configfs entries created; no error |

---

### 1.10 Concurrent Operations (cross-cutting within Agent gRPC)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 49 | `TestAgentServer_ConcurrentExportUnexport` | Concurrent Export + Unexport of same volume doesn't deadlock or corrupt | Two goroutines: one ExportVolume, one UnexportVolume simultaneously | Both complete within 5 s; final configfs state is either exported or clean |
| 50 | `TestException_ConcurrentExportUnexport` | XC8: Concurrent ExportVolume + UnexportVolume with real configfs state doesn't corrupt | Pre-establish export; goroutine A re-exports (idempotent), goroutine B unexports simultaneously | No deadlock; configfs state consistent (either exported or cleanly removed) |
| 51 | `TestConcurrentError_CreateVolume_SameID_NoDeadlock` | Concurrent CreateVolume requests for same VolumeID don't deadlock | N goroutines call CreateVolume with same VolumeID simultaneously | All goroutines complete within timeout; no deadlock; no panic |
| 52 | `TestConcurrentError_AllowDenyInitiator_SameHost_Race` | Concurrent AllowInitiator + DenyInitiator for same host NQN on same volume | Export volume; allow initiator; then run Allow+Deny concurrently | Both goroutines complete within 5 s (no deadlock); errors acceptable |

---

### 1.11 Error Paths: Context Cancellation and Deadline (cross-cutting within Agent gRPC)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 52 | `TestAgentErrors_ExportVolume_ContextCancelledDuringPoll` | Context deadline fires during device poll loop returns FailedPrecondition | DeviceChecker always returns false; request ctx timeout=100 ms; internal poll timeout=10 s | ExportVolume terminates within ~500 ms; returns gRPC FailedPrecondition |
| 53 | `TestAgentErrors_CreateVolume_BackendContextError` | Backend propagates context-related error | Mock backend returns context-type error | Returns non-OK gRPC status; server does not panic |
| 54 | `TestException_GRPCDeadlineExceeded` | gRPC request deadline is propagated through to backend invocation | Request context with 1 ms deadline; backend check respects context | Returns gRPC DeadlineExceeded or Canceled |

---

### 1.12 Error Paths: TOCTOU and Partial Failure (cross-cutting within Agent gRPC)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 55 | `TestAgentErrors_ExportVolume_ConfigfsBrokenAfterDeviceCheck_TOCTOU` | TOCTOU: configfs subsystems dir becomes read-only after device check succeeds | AlwaysPresentChecker; subsystems/ pre-created as read-only; tmpdir configfs | ExportVolume returns gRPC Internal (Apply failure); no panic |

---

### 1.13 Error Paths: Validation (cross-cutting within Agent gRPC)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 56 | `TestAgentErrors_CreateVolume_EmptyVolumeID` | Empty VolumeID on CreateVolume returns InvalidArgument | VolumeID="" in request | Returns gRPC InvalidArgument |
| 57 | `TestAgentErrors_DeleteVolume_EmptyVolumeID` | Empty VolumeID on DeleteVolume returns InvalidArgument | VolumeID="" in request | Returns gRPC InvalidArgument |
| 58 | `TestAgentErrors_ExpandVolume_EmptyVolumeID` | Empty VolumeID on ExpandVolume returns InvalidArgument | VolumeID="" in request | Returns gRPC InvalidArgument |
| 59 | `TestAgentErrors_ExpandVolume_ShrinkRejected_PropagatesAsInternal` | Shrink-rejected error from backend propagates as Internal to caller | Mock backend: expandErr contains "cannot be decreased" | Returns gRPC Internal; message contains backend error detail |
| 60 | `TestAgentErrors_ExportVolume_InvalidProtocol_NoConfigfsSideEffects` | Unsupported protocol leaves configfs untouched | protocol_type=ISCSI; fresh tmpdir configfs | Returns gRPC Unimplemented; no files created in configfs |
| 61 | `TestAgentErrors_AllowInitiator_InvalidProtocol` | Invalid protocol for AllowInitiator returns error without touching configfs | protocol_type=PROTOCOL_TYPE_ISCSI; exported volume in tmpdir | Returns non-OK gRPC status; no side-effects |
| 62 | `TestAgentErrors_DenyInitiator_InvalidProtocol` | Invalid protocol for DenyInitiator returns error | protocol_type=PROTOCOL_TYPE_ISCSI | Returns non-OK gRPC status |
| 63 | `TestAgentErrors_UnexportVolume_InvalidProtocol` | Invalid protocol for UnexportVolume returns error | protocol_type=PROTOCOL_TYPE_ISCSI | Returns non-OK gRPC status |
| 64 | `TestAgentErrors_CreateVolume_DiskFullPropagation` | Disk-full error propagates from backend through gRPC layer | Mock backend: createErr="out of space" | Returns gRPC ResourceExhausted or Internal; error message preserves detail |
| 65 | `TestAgentErrors_ExportVolume_MissingNvmeofTcpParams` | Missing NVMe-oF TCP export params returns InvalidArgument | NVMEOF_TCP protocol selected but export_params.nvmeof_tcp is nil | Returns gRPC InvalidArgument |

---

## Component 2: ZFS Backend (`internal/agent/backend/zfs/`)

**Subsystem-boundary setup:** `zfs.Backend` constructed with a sequential mock
executor (`seqExec`) that replays preset command outputs.  No real ZFS processes.
Cross-cutting failure paths (context cancellation, TOCTOU-like readback failures,
invalid properties) are tested here because they exercise the ZFS command logic.

---

### 2.1 Create

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 1 | `TestZFSBackend_Create_Success` | Creates zvol with correct zfs command; returns device path and allocated size | seqExec: existence check→"not found"; `zfs create -V 10G tank/pvc-abc`→ok; `zfs list` readback→"10G" | Returns devicePath="/dev/zvol/tank/pvc-abc", allocatedBytes=10 GiB; no error |
| 2 | `TestZFSBackend_Create_Idempotent` | Re-create of existing same-size volume returns existing info | seqExec: existence check returns volume at 10G | Returns ConflictError or existing size; no destructive command |
| 3 | `TestZFSBackend_Create_ConflictDifferentSize` | Existing zvol with different capacity returns ConflictError | seqExec: existence check returns 10G but requested 20G | Returns ConflictError with ExistingBytes=10G and RequestedBytes=20G |
| 4 | `TestZFSBackend_Create_DiskFull` | ENOSPC from zfs create propagates as error | seqExec: existence check→"not found"; `zfs create`→"out of space" | Returns non-nil error containing "out of space" |
| 5 | `TestZFSBackend_Create_PoolOffline` | Pool offline during create returns error | seqExec: existence check→"pool is not available" | Returns non-nil error; no create command issued |
| 6 | `TestZFSBackend_Create_WithParentDataset` | Parent dataset is incorporated into dataset path | Backend constructed with pool="tank", parentDataset="k8s" | zfs create command uses dataset path "tank/k8s/pvc-abc" |
| 7 | `TestZFSBackend_Create_WithProperties` | ZFS properties are forwarded verbatim to create command | ZfsVolumeParams with compression="lz4" and sync="disabled" | zfs create command includes `-o compression=lz4 -o sync=disabled` |

---

### 2.2 Delete

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 8 | `TestZFSBackend_Delete_Success` | Issues correct `zfs destroy` command and returns nil | seqExec: `zfs destroy tank/pvc-abc`→ok | Returns nil; no error |
| 9 | `TestZFSBackend_Delete_Idempotent` | Deleting non-existent dataset returns nil | seqExec: `zfs destroy`→"dataset does not exist" | Returns nil (idempotent) |
| 10 | `TestZFSBackend_Delete_DatasetBusy` | Busy dataset error propagates | seqExec: `zfs destroy`→"dataset is busy" | Returns non-nil error containing "busy" |
| 11 | `TestZFSBackend_Delete_ZFSError` | Generic ZFS error on destroy propagates | seqExec: `zfs destroy`→exit status 1 (generic) | Returns non-nil error |

---

### 2.3 Expand

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 12 | `TestZFSBackend_Expand_Success` | Expands zvol to new size; returns actual allocated bytes | seqExec: `zfs set volsize=20G tank/pvc-abc`→ok; `zfs list` readback→"20G" | Returns allocatedBytes=20 GiB; no error |
| 13 | `TestZFSBackend_Expand_ShrinkAttempt` | Shrink attempt returns error | seqExec: `zfs set volsize`→"volsize cannot be decreased" | Returns non-nil error |
| 14 | `TestZFSBackend_Expand_NotFound` | Expanding non-existent zvol returns error | seqExec: `zfs set volsize`→"dataset does not exist" | Returns non-nil error |
| 15 | `TestZFSBackend_Expand_ZFSError` | Generic ZFS error on volsize set propagates | seqExec: `zfs set volsize`→exit status 1 | Returns non-nil error |

---

### 2.4 Capacity

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 16 | `TestZFSBackend_Capacity_Success` | Parses pool capacity from `zpool list` output | seqExec: `zpool list -H -p -o size,free tank`→"107374182400\t64424509440" | Returns totalBytes=100 GiB, availableBytes=60 GiB; no error |
| 17 | `TestZFSBackend_Capacity_PoolOffline` | Pool offline returns error | seqExec: `zpool list`→"pool unavailable" | Returns non-nil error |
| 18 | `TestZFSBackend_Capacity_ParseError` | Empty or malformed output returns error gracefully | seqExec: `zpool list`→"" (empty) | Returns non-nil error; no panic |
| 19 | `TestZFSBackend_Capacity_ParseErrorNumeric` | Non-numeric capacity output returns parse error | seqExec: `zpool list`→"notanumber\tnotanumber" | Returns non-nil error; no panic |

---

### 2.5 ListVolumes

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 20 | `TestZFSBackend_ListVolumes_Success` | Lists all zvols from `zfs list` output | seqExec: `zfs list -H -p -t volume -r tank`→3-row output | Returns 3 VolumeInfo entries with correct IDs and sizes |
| 21 | `TestZFSBackend_ListVolumes_Empty` | Empty pool returns empty slice | seqExec: `zfs list`→header-only or empty output | Returns empty slice; no error |
| 22 | `TestZFSBackend_ListVolumes_ManyVolumes` | Large volume list handled without truncation | seqExec: `zfs list`→100-row output | Returns 100 VolumeInfo entries; no error |
| 23 | `TestZFSBackend_ListVolumes_ParentDatasetMissing` | Missing parent dataset returns empty or error | seqExec: `zfs list -r tank/k8s`→"dataset does not exist" | Returns empty slice or non-nil error; no panic |
| 24 | `TestZFSBackend_ListVolumes_ParseError` | Malformed `zfs list` output returns error | seqExec: `zfs list`→garbled non-tabular output | Returns non-nil error; no panic |

---

### 2.6 DevicePath

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 25 | `TestZFSBackend_DevicePath_Simple` | Device path for simple pool/volume is correct | Backend with pool="tank", no parentDataset; volumeID="tank/pvc-abc" | Returns "/dev/zvol/tank/pvc-abc" |
| 26 | `TestZFSBackend_DevicePath_WithParentDataset` | Device path incorporates parent dataset | Backend with pool="tank", parentDataset="k8s"; volumeID="tank/k8s/pvc-abc" | Returns "/dev/zvol/tank/k8s/pvc-abc" |

---

### 2.7 Context Cancellation (cross-cutting within ZFS Backend)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 27 | `TestZFSBackend_ContextCancelled` | Context cancellation propagates through Create | Backend with blocking executor; ctx cancelled before response | Returns ctx.Err() or wrapped cancellation error |
| 28 | `TestZFSBackend_Error_ContextCancelled_Delete` | Context cancellation during Delete propagates | Backend with blocking executor; ctx cancelled before response | Returns non-nil error; no hang |
| 29 | `TestZFSBackend_Error_ContextCancelled_Expand` | Context cancellation during Expand propagates | Backend with blocking executor; ctx cancelled before response | Returns non-nil error; no hang |
| 30 | `TestException_ZFSCommandTimeout` | ZFS command times out when context deadline is exceeded | Mock executor blocks until ctx.Done(); request ctx has short deadline | Backend method returns ctx.Err() within deadline |
| 38 | `TestZFSBackend_Error_ContextCancelled_ListVolumes` | Context cancellation during ListVolumes propagates | Backend with blocking executor; ctx cancelled before 'zfs list' response | Returns non-nil error wrapping context.Canceled; no hang |
| 39 | `TestZFSBackend_Error_ContextCancelled_Capacity` | Context cancellation during Capacity propagates | Backend with blocking executor; ctx cancelled before 'zpool list' response | Returns non-nil error wrapping context.Canceled; no hang |

---

### 2.8 Error Paths: Disk Full and Device Busy (cross-cutting within ZFS Backend)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 31 | `TestZFSBackend_Error_DiskFull_Expand` | Disk-full error from `zfs set volsize` propagates with message | seqExec: `zfs set volsize`→"out of space" | Returns non-nil error; error message contains "out of space" |
| 32 | `TestZFSBackend_Error_DeviceBusy_ExpandFails` | Device busy error during expand propagates | seqExec: `zfs set volsize`→"device busy" | Returns non-nil error; error message contains "device busy" |

---

### 2.9 Error Paths: Invalid Properties (cross-cutting within ZFS Backend)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 33 | `TestZFSBackend_Error_InvalidProperties` | Unrecognised ZFS property rejected by zfs(8) propagates | seqExec: existence check→"not found"; `zfs create -o bad-property=value`→"bad property: invalid property" | Returns non-nil error; message contains "bad property" |

---

### 2.10 Error Paths: Pool State Errors (cross-cutting within ZFS Backend)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 34 | `TestZFSBackend_Error_PoolOffline_ListVolumes` | Pool offline on ListVolumes propagates | seqExec: `zfs list`→"pool unavailable" | Returns non-nil error |
| 35 | `TestZFSBackend_Error_PoolFaulted_Capacity` | Faulted pool on Capacity propagates | seqExec: `zpool list`→"pool: FAULTED" error | Returns non-nil error |

---

### 2.11 Error Paths: Partial-Failure Readback (cross-cutting within ZFS Backend)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 36 | `TestZFSBackend_Error_CreateReadback_DatasetGone` | Dataset disappears between create and size readback | seqExec: create→ok; `zfs list` readback→"dataset does not exist" | Returns non-nil error |
| 37 | `TestZFSBackend_Error_ExpandReadback_PoolFailed` | Pool fails between expand command and size readback | seqExec: `zfs set volsize`→ok; `zfs list` readback→"pool failed" | Returns non-nil error |

---

## Component 3: NVMe-oF configfs Target (`internal/agent/nvmeof/`)

**Subsystem-boundary setup:** `nvmeof.NvmetTarget` with `ConfigfsRoot=t.TempDir()`.
Real filesystem operations on a tmpfs directory substitute for
`/sys/kernel/config/nvmet`.  Cross-cutting failure paths (permission denied,
concurrent apply, TOCTOU symlink races) are tested here.

---

### 3.1 Apply (Create configfs entries)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 1 | `TestNvmeof_Apply_FullLifecycle` | Apply creates all required configfs dirs and files | Fresh tmpdir; valid NvmetTarget | subsystems/<nqn>/, namespaces/1/, ports/<id>/ created; device_path, enable, addr_* files written |
| 2 | `TestNvmeof_Apply_Idempotent` | Calling Apply twice on same target is a no-op | Apply called twice | Same configfs state; no error on second call |
| 3 | `TestNvmeof_Apply_PartialFailureMidApply` | Partial configfs write is recoverable | Port dir creation blocked (pre-existing regular file at ports/ path) | Apply returns error; subsequent Apply with corrected tmpdir succeeds |
| 4 | `TestNvmeof_Apply_ACLEnabled` | When AllowedHosts set, attr_allow_any_host is "0" | NvmetTarget with non-empty AllowedHosts slice | attr_allow_any_host file contains "0\n" |
| 5 | `TestNvmeof_Apply_ACLDisabled` | When AllowedHosts empty, attr_allow_any_host is "1" | NvmetTarget with empty AllowedHosts | attr_allow_any_host file contains "1\n" |
| 6 | `TestNvmeof_Apply_DefaultPort` | Apply uses default port ID when Port=4420 | NvmetTarget with no explicit port ID override | Port dir created with deterministic port ID; port files written |
| 7 | `TestNvmeof_Apply_Remove_WithACL` | Full ACL lifecycle: apply with hosts, then remove | Apply with AllowedHosts=["host-nqn"]; then Remove | After Remove, subsystem dir and ACL symlinks are gone |

---

### 3.2 Remove (Clean up configfs entries)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 8 | `TestNvmeof_Remove_FullCleanup` | Remove deletes all configfs entries created by Apply | Apply then Remove | subsystems/<nqn>/ and port subsystem symlink removed; no orphan dirs |
| 9 | `TestNvmeof_Remove_Idempotent` | Remove on non-existent subsystem is a no-op | No prior Apply | Returns nil; no error |
| 10 | `TestNvmeof_Remove_AlreadyRemovedSubsystem` | Partial removal state handled | Subsystem dir missing but port subsystem symlink exists | Cleans up remaining entries; no error |

---

### 3.3 ACL: AllowHost / DenyHost

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 11 | `TestNvmeof_AllowHost_CreatesSymlink` | Granting access creates allowed_hosts symlink | Apply with ACL enabled; AllowHost with host NQN | allowed_hosts/<host-nqn> symlink created pointing to hosts/<host-nqn>/ |
| 12 | `TestNvmeof_AllowHost_Idempotent` | Allowing same host twice creates single symlink | AllowHost called twice with same host NQN | Single symlink; no error |
| 13 | `TestNvmeof_AllowHost_MultipleHosts` | Multiple different hosts can be allowed concurrently | AllowHost for 3 different host NQNs | 3 symlinks in allowed_hosts/ |
| 14 | `TestNvmeof_AllowHost_SymlinkWrongTarget` | Existing symlink with wrong target is corrected | Pre-create allowed_hosts/<nqn> pointing to wrong path | AllowHost corrects or replaces the symlink; returns nil |
| 15 | `TestNvmeof_DenyHost_RemovesSymlink` | Revoking access removes allowed_hosts symlink | Apply; AllowHost; DenyHost same host | Symlink removed from allowed_hosts/; hosts/<nqn> dir may remain |
| 16 | `TestNvmeof_DenyHost_Idempotent` | Denying non-allowed host is a no-op | DenyHost without prior AllowHost | Returns nil; no error |

---

### 3.4 Port Management

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 17 | `TestNvmeof_Port_MultipleSubsystemsSamePort` | Two subsystems sharing same port each get their own subsystem symlink | Apply two NvmetTargets with same address/port but different NQNs | Two subsystem symlinks under ports/<id>/subsystems/ |
| 18 | `TestNvmeof_Port_SeparatePortsForDifferentAddresses` | Different bind addresses use separate port directories | Apply two NvmetTargets with different bind addresses | Two separate port directories in ports/ |

---

### 3.5 Exports Scanning: ListExports

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 19 | `TestNvmeof_ListExports_Success` | Scanning configfs returns all active exports | Two volumes applied in tmpdir | Returns 2 ExportedSubsystem entries with correct NQNs and device paths |
| 20 | `TestNvmeof_ListExports_Empty` | Fresh configfs returns empty list | tmpdir with empty subsystems/ dir | Returns empty slice; no error |
| 21 | `TestNvmeof_ListExports_NoSubsystemsDir` | Missing subsystems directory handled gracefully | tmpdir without any nvmet/ subdirs | Returns empty slice or no error |
| 22 | `TestNvmeof_ListExports_PartialSubsystem` | Subsystem without namespace is handled | Subsystem dir created without namespaces/ child | Returns ExportedSubsystem with empty device paths or skipped entry |
| 23 | `TestNvmeof_ListExports_WithAllowedHosts` | AllowedHosts field populated from symlinks | Subsystem applied with 2 allowed_hosts symlinks | ExportedSubsystem.AllowedHosts has 2 entries |
| 24 | `TestNvmeof_ListExports_RoundTrip` | Apply then ListExports returns equivalent targets | Apply an NvmetTarget; call ListExports | ListExports entry matches the original NvmetTarget fields |

---

### 3.6 Device Polling

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 25 | `TestNvmeof_DevicePoll_AppearsImmediately` | Device already present; no retry needed | DeviceChecker returns true immediately | Returns without any retry; device path returned promptly |
| 26 | `TestNvmeof_DevicePoll_AppearsAfterDelay` | Device appears after several poll attempts | DeviceChecker returns false twice, then true | Returns device path after retries; no error |
| 27 | `TestNvmeof_DevicePoll_NeverAppears` | Device never appears; context timeout | DeviceChecker always returns false; short-deadline context | Returns error after context deadline; no infinite loop |
| 28 | `TestNvmeof_DevicePoll_ContextCancelled` | Context cancelled before device appears | ctx cancelled externally during poll | Returns context error promptly |
| 29 | `TestNvmeof_DevicePoll_PermissionDenied` | Permission denied on device path propagates immediately | DeviceChecker returns os.ErrPermission | Returns error immediately without further retries |

---

### 3.7 Error Paths: Permission Denied (cross-cutting within NVMe-oF)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 30 | `TestException_ConfigfsWritePermissionDenied` | Write to read-only configfs subsystems dir fails gracefully | Pre-create nvmet/subsystems/ with mode 0555 in tmpdir | Apply returns non-nil error; no panic |
| 31 | `TestNvmeof_Error_Apply_PortsDirPermissionDenied` | Apply fails when ports parent dir is read-only | Pre-create nvmet/ports/ with mode 0555 | Apply returns non-nil error; subsystem dir may be partially created |
| 32 | `TestNvmeof_Error_AllowHost_HostsDirPermissionDenied` | AllowHost fails when hosts/ dir is read-only | Pre-create subsystem hosts/ with mode 0444 | AllowHost returns non-nil error |
| 33 | `TestNvmeof_Error_AllowHost_AllowedHostsDirPermissionDenied` | AllowHost fails when allowed_hosts/ dir is read-only | Pre-create subsystem allowed_hosts/ with mode 0444 | AllowHost returns non-nil error |
| 34 | `TestNvmeof_Error_Apply_EnableFileReadOnly` | Apply fails when enable file cannot be written | Pre-create namespace enable file with mode 0444 | Apply returns non-nil error |

---

### 3.8 Error Paths: Partial Apply and File-Conflicts (cross-cutting within NVMe-oF)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 35 | `TestNvmeof_Error_PartialApply_NamespaceBlockedByFile` | Namespace dir creation blocked by a regular file at that path | Pre-create namespaces/1 as a regular file | Apply returns non-nil error |
| 36 | `TestConcurrentError_Apply_AttrAllowAnyHostReadOnly` | Apply fails when attr_allow_any_host file is read-only | Pre-create attr_allow_any_host with mode 0444 | Apply returns non-nil error |
| 37 | `TestConcurrentError_DenyHost_PathIsRegularFile` | DenyHost on path occupied by regular file returns error | Pre-create allowed_hosts/<nqn> as a regular file instead of symlink | DenyHost returns non-nil error; no panic |
| 38 | `TestConcurrentError_Apply_PortSubsystemsBlockedByFile` | Apply fails when port subsystems/ path is a regular file | Pre-create ports/<id>/subsystems as a regular file | Apply returns non-nil error |

---

### 3.9 Concurrent Operations (cross-cutting within NVMe-oF)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 39 | `TestException_ConfigfsDirCreateRace` | Concurrent Apply calls sharing same port dir don't corrupt state | N goroutines each with unique NQN but same port; Apply simultaneously | All goroutines complete without error; final configfs state consistent |
| 40 | `TestException_SymlinkWrongDestination` | Existing symlink with stale target is replaced | Pre-create allowed_hosts/<nqn> pointing to wrong target | AllowHost corrects symlink or returns error; no corruption |
| 41 | `TestException_PartialConfigfsWrite` | Partial write detected (write returns short count) | Mock OS that returns short write on configfs attr file | NvmetTarget detects short write; returns error |
| 42 | `TestException_DeviceTOCTOU` | Device disappears between poll-check and configfs-write | DeviceChecker returns true; then device check at Apply time fails | Returns appropriate error |
| 43 | `TestConcurrentError_AllowDenyInitiator_SameHost_Race` | Concurrent AllowHost + DenyHost for same NQN doesn't deadlock | AllowHost and DenyHost goroutines run simultaneously on same subsystem | Both complete within 5 s; no deadlock; no panic |
| 44 | `TestConcurrentError_Remove_SharedPort_NoDeadlock` | Concurrent Remove calls on different subsystems sharing one port | Two goroutines Remove different subsystems that share port dir | Both complete without deadlock; port symlinks cleaned up |
| 45 | `TestConcurrentError_AllowHost_SameHost_Idempotent` | Concurrent AllowHost for same host NQN returns nil for all goroutines | N goroutines simultaneously AllowHost same host NQN | All goroutines return nil; single symlink in allowed_hosts/ |

---

### 3.10 Error Paths: WaitForDevice Edge Cases (cross-cutting within NVMe-oF)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 46 | `TestNvmeof_Error_WaitForDevice_PreCancelledContext` | Pre-cancelled context returns immediately | ctx already cancelled before WaitForDevice call | Returns context error immediately without polling |
| 47 | `TestNvmeof_Error_WaitForDevice_ShortTimeoutNeverAppears` | Short timeout with device never appearing | Poll interval=5 ms; timeout=15 ms; DeviceChecker always false | Returns error within ~15–50 ms; no hang |

---

### 3.11 Port ID Determinism and Multi-target Port Reuse

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 48 | `TestNvmeof_Apply_PortID_Deterministic` | Same bind addr+port produces the same port dir name across independent Apply calls | Apply target1 (addr="10.0.0.1", port=4420) to tmpdir1; apply target2 (same addr+port, different NQN) to tmpdir2; compare port dir names | Both tmpdirs contain a port dir with identical names (same stable hash) |
| 49 | `TestNvmeof_Apply_PortID_DifferentForDifferentAddresses` | Different bind addresses produce separate port directories | Apply two NvmetTargets with same port but different BindAddress values to one shared tmpdir | Two distinct port dirs under nvmet/ports/; each has correct addr_traddr value |
| 50 | `TestNvmeof_Apply_ReusesSamePortDir` | Two subsystems sharing same addr:port share one port dir with two symlinks | Apply two NvmetTargets with same BindAddress+Port but different NQNs to one shared tmpdir | Exactly one port dir; two subsystem symlinks inside ports/<id>/subsystems/ |
| 51 | `TestNvmeof_Apply_NamespaceIDNonDefault` | NamespaceID=5 creates namespaces/5/ directory, not namespaces/1/ | NvmetTarget{NamespaceID: 5, …}; Apply | namespaces/5/ dir created with device_path and enable files; no namespaces/1/ |

---

### 3.12 Remove Lifecycle Edge Cases

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 52 | `TestNvmeof_Remove_LeavesPortDirIntact` | Remove deletes subsystem's port symlink but not the port directory itself | Apply; Remove; inspect ports/ | subsystem dir gone; ports/<id>/ dir still present under nvmet/ports/ |
| 53 | `TestNvmeof_Remove_LeavesHostsDirIntact` | Remove does not clean the global hosts/ directory | Apply; AllowHost("host-nqn"); Remove | nvmet/hosts/ dir still present; hosts/host-nqn/ still exists after Remove |
| 54 | `TestNvmeof_Remove_PortLinkAlreadyGone` | Remove succeeds even when the port subsystem symlink is already absent | Apply; manually delete port symlink; Remove | Returns nil; subsystem dir cleaned; no error |
| 55 | `TestNvmeof_Remove_SuccessAfterApplyWithNoHosts` | Apply with no ACL hosts followed by Remove leaves clean state | Apply with empty AllowedHosts; Remove | All created dirs removed; no dangling entries in subsystems/ or namespaces/ |

---

### 3.13 AllowHost / DenyHost Edge Cases

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 56 | `TestNvmeof_AllowHost_CreatesHostDir` | AllowHost creates the global hosts/<hostNQN>/ directory if absent | Apply; AllowHost("host-nqn") | hosts/host-nqn/ dir exists under nvmet/ after AllowHost |
| 57 | `TestNvmeof_AllowHost_HostDirPreExists` | AllowHost is idempotent when hosts/<nqn>/ dir pre-exists | Pre-create nvmet/hosts/host-nqn/ manually; AllowHost("host-nqn") | Returns nil; allowed_hosts/<nqn> symlink created; no duplicate-dir error |
| 58 | `TestNvmeof_DenyHost_LeavesHostDirIntact` | DenyHost removes allowed_hosts symlink but leaves hosts/<nqn>/ dir | Apply; AllowHost; DenyHost same host | allowed_hosts/<nqn> symlink gone; hosts/host-nqn/ dir still present |
| 59 | `TestNvmeof_AllowHost_AllowedHostsDirCreated` | AllowHost creates allowed_hosts/ dir inside subsystem if it is absent | Apply; directly call AllowHost (allowed_hosts/ not pre-created) | allowed_hosts/ dir exists under subsystem dir; symlink inside it |

---

### 3.14 ListExports Advanced Scanning

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 60 | `TestNvmeof_ListExports_MultipleNamespaces` | Subsystem with two namespace dirs reports both device paths | Manually create subsystem dir with namespaces/1 and namespaces/2, each containing a device_path file | ExportedSubsystem.DevicePaths map has 2 entries with correct per-namespace paths |
| 61 | `TestNvmeof_ListExports_DevicePathFileEmpty` | Namespace with empty device_path file is handled without panic | Apply NvmetTarget with DevicePath=""; call ListExports | ListExports returns entry with empty string DevicePath; no panic |
| 62 | `TestNvmeof_ListExports_NQNFromDirName` | ListExports returns the exact NQN string taken from the directory name | Apply with NQN "nqn.test:pvc-unique"; call ListExports | ExportedSubsystem.SubsystemNQN == "nqn.test:pvc-unique" |

---

### 3.15 Apply Field Verification

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 63 | `TestNvmeof_Apply_DevicePathVerified` | device_path file content exactly matches NvmetTarget.DevicePath | Apply with DevicePath="/dev/zvol/pool/pvc-abc" | File at namespaces/1/device_path contains "/dev/zvol/pool/pvc-abc" verbatim |
| 64 | `TestNvmeof_Apply_EnableFileContainsOne` | enable file contains "1" after a successful Apply | Apply NvmetTarget | namespaces/1/enable file contains exactly "1" |
| 65 | `TestNvmeof_Apply_AddrTrsvcidMatchesPort` | addr_trsvcid file content matches NvmetTarget.Port as decimal string | Apply with Port=9500 | ports/<id>/addr_trsvcid file contains "9500" |

---

## Component 4: CSI Controller Service (`internal/csi/`)

**Subsystem-boundary setup:** `csi.ControllerServer` with mock
`agentv1.AgentServiceClient`.  CSI error paths (agent deadline, unreachable,
AlreadyExists) are tested within this component.

---

### 4.1 CreateVolume

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 1 | `TestCSIController_CreateVolume_Success` | Full CreateVolume flow creates backend and export | Mock agent: CreateVolume→OK, ExportVolume→OK | Returns CreateVolumeResponse with volume_id and context |
| 2 | `TestCSIController_CreateVolume_CapacityRange` | Respects limit_bytes in capacity range | Request with limit_bytes=10 GiB | Returns volume sized within [required_bytes, limit_bytes] |
| 3 | `TestCSIController_CreateVolume_IdempotentRetry` | Identical CreateVolume request can be retried | Same name and capacity called twice | Both calls succeed; same volume returned |
| 4 | `TestCSIController_CreateVolume_AgentError` | Agent generic error maps to Internal | Mock agent: CreateVolume→gRPC Internal | Returns gRPC Internal |
| 5 | `TestCSIController_CreateVolume_AgentUnreachable` | Agent unreachable maps to non-OK status | Mock agent: CreateVolume→gRPC Unavailable | Returns non-OK gRPC status |
| 6 | `TestCSIController_CreateVolume_MissingName` | Missing volume name returns InvalidArgument | Name="" in request | Returns gRPC InvalidArgument |
| 7 | `TestCSIController_CreateVolume_TargetNotFound` | Target node not found returns NotFound | Mock agent lookup fails for target | Returns gRPC NotFound or Internal |
| 8 | `TestCSIController_CreateVolume_MissingParams` | Missing required storage parameters returns InvalidArgument | No pool parameter in volume context | Returns gRPC InvalidArgument |
| 9 | `TestCSIController_CreateVolume_DuplicateName` | Same name with conflicting capacity returns AlreadyExists | Mock agent: CreateVolume→gRPC AlreadyExists | Returns gRPC AlreadyExists |

---

### 4.2 DeleteVolume

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 10 | `TestCSIController_DeleteVolume_Success` | Deletes both ACL, export and backend storage | Mock agent: UnexportVolume→OK, DeleteVolume→OK | Returns empty DeleteVolumeResponse |
| 11 | `TestCSIController_DeleteVolume_Idempotent` | Deleting already-deleted volume is OK | Mock agent: UnexportVolume→NotFound, DeleteVolume→NotFound | Returns empty response; no error |
| 12 | `TestCSIController_DeleteVolume_AgentError` | Agent delete error propagates | Mock agent: DeleteVolume→gRPC Internal | Returns non-OK gRPC status |

---

### 4.3 ControllerPublishVolume / ControllerUnpublishVolume

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 13 | `TestCSIController_ControllerPublishVolume_Success` | Publishes volume by allowing initiator on agent | Mock agent: AllowInitiator→OK | Returns ControllerPublishVolumeResponse with publish context |
| 14 | `TestCSIController_ControllerPublishVolume_AlreadyPublished` | Re-publishing already-published volume is idempotent | Mock agent: AllowInitiator→OK (idempotent) | Returns publish context; no error |
| 15 | `TestCSIController_ControllerUnpublishVolume_Success` | Unpublishes volume by denying initiator on agent | Mock agent: DenyInitiator→OK | Returns empty ControllerUnpublishVolumeResponse |
| 16 | `TestCSIController_ControllerUnpublishVolume_AlreadyUnpublished` | Unpublishing already-unpublished volume is a no-op | Mock agent: DenyInitiator→NotFound | Returns empty response; no error |

---

### 4.4 ControllerExpandVolume

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 17 | `TestCSIController_ExpandVolume_Success` | Expands volume through agent | Mock agent: ExpandVolume→OK with new capacity | Returns ControllerExpandVolumeResponse with capacity_bytes |
| 18 | `TestCSIController_ExpandVolume_AgentError` | Agent expand error propagates | Mock agent: ExpandVolume→gRPC Internal | Returns non-OK gRPC status |

---

### 4.5 ValidateVolumeCapabilities / GetCapabilities

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 19 | `TestCSIController_ValidateVolumeCapabilities_Supported` | Supported access mode returns confirmed capabilities | Request with SINGLE_NODE_WRITER access mode | Returns confirmed capabilities; no error |
| 20 | `TestCSIController_ValidateVolumeCapabilities_Unsupported` | Unsupported access mode returns error | Request with MULTI_NODE_MULTI_WRITER | Returns error or empty confirmed capabilities |
| 21 | `TestCSIController_GetCapabilities` | Returns controller capability list | Server constructed | Response includes CREATE_DELETE_VOLUME, PUBLISH_UNPUBLISH_VOLUME |

---

### 4.6 Error Paths (cross-cutting within CSI Controller)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 22 | `TestCSIErrors_CreateVolume_AgentDeadlineExceeded` | Agent deadline exceeded on CreateVolume propagates | Mock agent: CreateVolume→gRPC DeadlineExceeded | Returns gRPC DeadlineExceeded or Internal |
| 23 | `TestCSIErrors_CreateVolume_AgentUnreachable_PlainError` | Agent plain (non-gRPC) error on CreateVolume | Mock agent: CreateVolume→plain Go error | Returns non-OK gRPC status |
| 24 | `TestCSIErrors_ControllerExpand_ShrinkRejected` | Shrink-rejected on ControllerExpandVolume | Mock agent: ExpandVolume→gRPC InvalidArgument or Internal | Returns non-OK gRPC status |
| 25 | `TestCSIErrors_ControllerExpand_AgentDeadlineExceeded` | Agent deadline on ControllerExpandVolume | Mock agent: ExpandVolume→gRPC DeadlineExceeded | Returns gRPC DeadlineExceeded or Internal |
| 26 | `TestCSIErrors_ControllerPublish_AllowInitiatorFails` | AllowInitiator error on ControllerPublishVolume | Mock agent: AllowInitiator→gRPC Internal | Returns non-OK gRPC status |
| 27 | `TestCSIErrors_DeleteVolume_AgentDeadlineExceeded` | Agent deadline on DeleteVolume | Mock agent: DeleteVolume→gRPC DeadlineExceeded | Returns non-OK gRPC status |
| 28 | `TestCSIErrors_CreateVolume_InvalidCapabilities_Nil` | Nil volume capabilities returns InvalidArgument | VolumeCapabilities=nil in request | Returns gRPC InvalidArgument |
| 29 | `TestCSIErrors_ControllerExpand_InvalidArgument_EmptyVolumeID` | Empty VolumeID on ControllerExpandVolume | VolumeID="" in request | Returns gRPC InvalidArgument |
| 30 | `TestCSIErrors_CreateVolume_MissingVolumeCapabilities_Empty` | Empty volume capabilities slice returns InvalidArgument | VolumeCapabilities=[] (empty slice) | Returns gRPC InvalidArgument |

---

### 4.7 Input Validation Edge Cases (cross-cutting within CSI Controller)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 31 | `TestCSIController_ControllerPublishVolume_EmptyVolumeID` | Empty VolumeID on ControllerPublishVolume returns InvalidArgument | VolumeID="" in request | Returns gRPC InvalidArgument; no agent call |
| 32 | `TestCSIController_ControllerPublishVolume_EmptyNodeID` | Empty NodeID on ControllerPublishVolume returns InvalidArgument | NodeID="" in request; valid VolumeID | Returns gRPC InvalidArgument; no agent call |
| 33 | `TestCSIController_ControllerPublishVolume_NilVolumeCapability` | Nil volume_capability on ControllerPublishVolume returns InvalidArgument | VolumeCapability=nil; valid VolumeID and NodeID | Returns gRPC InvalidArgument |
| 34 | `TestCSIController_ControllerPublishVolume_MalformedVolumeID` | Malformed volumeID (no slashes) returns InvalidArgument | VolumeID="badformat"; valid NodeID and capability | Returns gRPC InvalidArgument |
| 35 | `TestCSIController_ControllerUnpublishVolume_EmptyVolumeID` | Empty VolumeID on ControllerUnpublishVolume returns InvalidArgument | VolumeID="" | Returns gRPC InvalidArgument |
| 36 | `TestCSIController_ControllerUnpublishVolume_EmptyNodeID` | Empty NodeID on ControllerUnpublishVolume returns success (no-op per CSI spec §4.3.4) | Valid VolumeID; NodeID="" | Returns empty ControllerUnpublishVolumeResponse; no agent DenyInitiator call |
| 37 | `TestCSIController_ControllerUnpublishVolume_MalformedVolumeID` | Malformed volumeID returns success (unknown volume treated as already removed) | VolumeID="badformat" | Returns empty response; no agent call |
| 38 | `TestCSIController_ExpandVolume_NilCapacityRange` | Nil capacity_range on ControllerExpandVolume returns InvalidArgument | CapacityRange=nil | Returns gRPC InvalidArgument |
| 39 | `TestCSIController_ExpandVolume_NegativeRequiredBytes` | Negative required_bytes returns InvalidArgument | CapacityRange.RequiredBytes=-1 | Returns gRPC InvalidArgument |
| 40 | `TestCSIController_ExpandVolume_MalformedVolumeID` | Malformed volumeID on ControllerExpandVolume returns InvalidArgument | VolumeID="bad-format-no-slashes"; valid capacity range | Returns gRPC InvalidArgument |

---

### 4.8 PillarTarget State Errors (cross-cutting within CSI Controller)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 41 | `TestCSIController_CreateVolume_TargetNoResolvedAddress` | PillarTarget exists but has empty ResolvedAddress returns Unavailable | PillarTarget seeded with empty Status.ResolvedAddress | Returns gRPC Unavailable |
| 42 | `TestCSIController_DeleteVolume_TargetNotFound` | PillarTarget missing during DeleteVolume returns success (decommissioned node) | Well-formed volume ID encoding a target name not in k8s store | Returns empty DeleteVolumeResponse; no error |
| 43 | `TestCSIController_DeleteVolume_TargetNoResolvedAddress` | PillarTarget found but no ResolvedAddress on DeleteVolume returns Unavailable | PillarTarget with empty ResolvedAddress; well-formed volume ID | Returns gRPC Unavailable |
| 44 | `TestCSIController_ControllerPublishVolume_TargetNotFound` | PillarTarget missing on ControllerPublishVolume returns NotFound | Valid VolumeID encoding target name not in k8s store | Returns gRPC NotFound |
| 45 | `TestCSIController_ControllerPublishVolume_TargetNoResolvedAddress` | PillarTarget exists but no ResolvedAddress on ControllerPublishVolume returns Unavailable | PillarTarget with empty ResolvedAddress; valid VolumeID and NodeID | Returns gRPC Unavailable |
| 46 | `TestCSIController_ExpandVolume_TargetNotFound` | PillarTarget missing on ControllerExpandVolume returns NotFound | Valid VolumeID encoding non-existent target | Returns gRPC NotFound |
| 47 | `TestCSIController_ExpandVolume_TargetNoResolvedAddress` | PillarTarget exists but no ResolvedAddress on ControllerExpandVolume returns Unavailable | PillarTarget with empty ResolvedAddress; valid VolumeID | Returns gRPC Unavailable |

---

### 4.9 Partial Failure Recovery and Agent Response Handling

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 48 | `TestCSIController_CreateVolume_ExportFails_RecordsCreatePartial` | ExportVolume failure after backend creation causes error and partial CRD state | Mock agent: CreateVolume→OK; ExportVolume→gRPC Internal | Returns gRPC Internal; PillarVolume CRD persisted in k8s with CreatePartial phase |
| 49 | `TestCSIController_ExpandVolume_AgentReturnsZeroBytes` | Agent ExpandVolume returning CapacityBytes=0 causes response to use requested_bytes as fallback | Mock agent: ExpandVolume→{CapacityBytes:0}; request required_bytes=20 GiB | Returns ControllerExpandVolumeResponse.CapacityBytes=20 GiB |
| 50 | `TestCSIErrors_ControllerUnpublish_DenyInitiatorNonNotFound` | DenyInitiator returning a non-NotFound error propagates to caller | Mock agent: DenyInitiator→gRPC Internal | Returns non-OK gRPC status (Internal); no success masking |

---

## Component 5: CSI Node Service (`internal/csi/`)

**Subsystem-boundary setup:** `csi.NodeServer` with mock `Connector`
(NVMe-oF connect/disconnect) and mock `Mounter` (format-and-mount / mount /
unmount).  Cross-cutting failure paths (TOCTOU device disappears, mount fails)
are tested here.

---

### 5.1 GetCapabilities / GetInfo

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 1 | `TestCSINode_GetCapabilities` | Returns node capabilities | NodeServer constructed | Response includes STAGE_UNSTAGE_VOLUME capability |
| 2 | `TestCSINode_GetInfo` | Returns node ID | nodeID="test-node" | Response.node_id="test-node" |

---

### 5.2 NodeStageVolume

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 3 | `TestCSINode_NodeStageVolume_MountAccess` | Stage volume with mount access mode connects and mounts | Mock connector: Connect→OK; mock mounter: FormatAndMount→OK | Returns empty NodeStageVolumeResponse |
| 4 | `TestCSINode_NodeStageVolume_BlockAccess` | Stage volume with block access mode connects only | Mock connector: Connect→OK; no mount expected | Returns empty NodeStageVolumeResponse |
| 5 | `TestCSINode_NodeStageVolume_AlreadyStaged` | Already staged volume is a no-op | Mock connector + mounter already staged state | Returns empty response; no error |
| 6 | `TestCSINode_NodeStageVolume_ConnectFails` | NVMe-oF connect error returns Internal | Mock connector: Connect→error | Returns non-OK gRPC status |
| 7 | `TestCSINode_NodeStageVolume_DeviceTimeout` | Device polling times out after connect | Mock connector: GetDevicePath never returns path | Returns gRPC FailedPrecondition or DeadlineExceeded |
| 8 | `TestCSINode_NodeStageVolume_GetDevicePathError` | GetDevicePath error propagates | Mock connector: GetDevicePath→error | Returns non-OK gRPC status |
| 9 | `TestCSINode_NodeStageVolume_MountFails` | Mount failure after connect returns Internal | Mock connector: OK; mock mounter: FormatAndMount→error | Returns gRPC Internal |
| 10 | `TestCSINode_NodeStageVolume_MissingVolumeContext` | Missing required volume context fields returns InvalidArgument | volume_context missing NQN or address | Returns gRPC InvalidArgument |
| 11 | `TestCSINode_NodeStageVolume_MissingVolumeID` | Missing VolumeID returns InvalidArgument | VolumeID="" | Returns gRPC InvalidArgument |

---

### 5.3 NodeUnstageVolume

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 12 | `TestCSINode_NodeUnstageVolume_Success` | Unmounts and disconnects NVMe-oF volume | Mock mounter: Unmount→OK; mock connector: Disconnect→OK | Returns empty NodeUnstageVolumeResponse |
| 13 | `TestCSINode_NodeUnstageVolume_AlreadyUnstaged` | Unstaging already-unstaged volume is a no-op | Mock mounter: IsMounted→false | Returns empty response; no error |

---

### 5.4 NodePublishVolume / NodeUnpublishVolume

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 14 | `TestCSINode_NodePublishVolume_Success` | Bind-mounts staging path to target path | Mock mounter: Mount→OK | Returns empty NodePublishVolumeResponse |
| 15 | `TestCSINode_NodePublishVolume_ReadOnly` | Read-only publish uses ro mount option | ReadOnly=true in request | Mount called with "ro" option |
| 16 | `TestCSINode_NodePublishVolume_Idempotent` | Already-published volume is a no-op | Mock mounter: IsMounted→true | Returns empty response; no error |
| 17 | `TestCSINode_NodePublishVolume_MountFails` | Mount failure returns Internal | Mock mounter: Mount→error | Returns gRPC Internal |
| 18 | `TestCSINode_NodeUnpublishVolume_Success` | Unmounts target path | Mock mounter: Unmount→OK | Returns empty NodeUnpublishVolumeResponse |
| 19 | `TestCSINode_NodeUnpublishVolume_Idempotent` | Unpublishing not-published volume is a no-op | Mock mounter: IsMounted→false | Returns empty response; no error |

---

### 5.5 Full Lifecycle

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 20 | `TestCSINode_NodeStageUnstagePublishUnpublish_FullLifecycle` | Complete volume lifecycle: stage → publish → unpublish → unstage | All mock dependencies succeed in sequence | Each RPC returns success; final state: unmounted and disconnected |

---

### 5.6 Error Paths (cross-cutting within CSI Node)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 21 | `TestCSIErrors_NodeStage_MkfsFailure` | Filesystem format failure returns Internal | Mock mounter: FormatAndMount→"mkfs failed" | Returns gRPC Internal |
| 22 | `TestCSIErrors_NodeStage_TOCTOU_DeviceDisappears` | TOCTOU: device disappears after connect but before mount | Mock connector: Connect→OK, GetDevicePath→ok once then not found | Returns non-OK gRPC status; no panic |
| 23 | `TestCSIErrors_NodePublish_BindMountFails` | Bind mount to target path fails | Mock mounter: Mount→"bind mount error" | Returns gRPC Internal |
| 24 | `TestCSIErrors_NodeUnstage_UnmountError` | Unmount failure during unstage | Mock mounter: Unmount→error | Returns non-OK gRPC status |
| 25 | `TestCSIErrors_NodeUnpublish_UnmountError` | Unmount failure during unpublish | Mock mounter: Unmount→error | Returns non-OK gRPC status |
| 26 | `TestCSIErrors_NodeStage_GRPCDeadlineExceeded` | gRPC deadline during NodeStageVolume propagates | Short-deadline ctx; connector blocks | Returns gRPC DeadlineExceeded |
| 27 | `TestCSIErrors_NodeStage_InvalidParams_MissingVolumeID` | Invalid params on NodeStageVolume | VolumeID="" | Returns gRPC InvalidArgument |
| 28 | `TestCSIErrors_NodeStage_ConnectFailure_PropagatesInternal` | NVMe-oF connect failure propagates as Internal | Mock connector: Connect→generic error | Returns gRPC Internal |

---

### 5.7 NodeExpandVolume

The `NodeExpandVolume` RPC is advertised via `EXPAND_VOLUME` in
`NodeGetCapabilities`.  The current `NodeServer` does not provide an explicit
implementation, so requests fall through to the embedded
`csi.UnimplementedNodeServer`, which returns `codes.Unimplemented`.  These
test cases verify the advertised capability is consistent with the actual RPC
behaviour and guard against silent regressions if the RPC is later implemented.

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 29 | `TestCSINode_NodeExpandVolume_CurrentlyUnimplemented` | NodeExpandVolume with current code returns Unimplemented without panic | NodeServer constructed; valid NodeExpandVolumeRequest with VolumeID and CapacityRange | Returns gRPC Unimplemented; no panic |
| 30 | `TestCSINode_GetCapabilities_AdvertisesExpandVolume` | GetCapabilities includes EXPAND_VOLUME alongside STAGE_UNSTAGE_VOLUME | NodeServer constructed; call NodeGetCapabilities | Response.Capabilities contains both STAGE_UNSTAGE_VOLUME and EXPAND_VOLUME RPC types |

---

### 5.8 State Machine Integration (cross-cutting within CSI Node)

`NodeServer` accepts an optional `VolumeStateMachine` (set via
`NewNodeServerWithStateMachine`).  When present, every node RPC validates the
volume lifecycle state before executing privileged work, rejecting out-of-order
calls with `FailedPrecondition`.  Tests in this section exercise the ordering
guards without touching the kernel.

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 31 | `TestCSINode_StateMachine_NodeStage_WrongOrder` | NodeStageVolume rejected when volume is not in ControllerPublished state | NodeServer with shared VolumeStateMachine; volume created but ControllerPublishVolume not yet called | Returns gRPC FailedPrecondition; Connect never called |
| 32 | `TestCSINode_StateMachine_NodePublish_WrongOrder` | NodePublishVolume rejected when volume is not in NodeStaged state | Volume in ControllerPublished state (not NodeStaged); NodePublishVolume called | Returns gRPC FailedPrecondition; Mount never called |
| 33 | `TestCSINode_StateMachine_NodeUnpublish_WrongOrder` | NodeUnpublishVolume is idempotent when volume is not in NodePublished state (per CSI spec §5.4.2) | Volume still in NodeStaged state; NodeUnpublishVolume called directly | Returns success without Unmount (idempotent no-op); if implementation returns error, it must be FailedPrecondition |
| 34 | `TestCSINode_StateMachine_FullLifecycleWithSM` | Correct ordering through full lifecycle with state machine succeeds | NodeServer with VolumeStateMachine; Stage → Publish → Unpublish → Unstage in order | Each RPC succeeds; no FailedPrecondition errors |

---

### 5.9 State File Edge Cases (cross-cutting within CSI Node)

`NodeServer` persists a JSON state file per staged volume in `stateDir` so that
`NodeUnstageVolume` can recover the subsystem NQN without the VolumeContext.
These tests exercise failure paths around that persistence mechanism.

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 35 | `TestCSINode_NodeUnstage_CorruptStateFile` | Corrupt (non-JSON) state file during unstage is handled without panic | Pre-write arbitrary bytes to state file path; call NodeUnstageVolume | Returns non-OK gRPC status; no panic; no infinite loop |
| 36 | `TestCSINode_NodeStage_StateDirUnwritable` | Unwritable stateDir causes NodeStageVolume to fail after mount succeeds | stateDir has mode 0555; connector and mounter succeed; call NodeStageVolume | Returns non-OK gRPC status; error mentions state file or stateDir; no panic |
| 37 | `TestCSINode_NodeUnstage_StateFileMissingIsOK` | Missing state file is treated as "not staged" — NodeUnstage becomes a no-op | Volume staged, state file manually deleted, IsMounted→false; call NodeUnstageVolume | Returns empty response; no error; Disconnect may or may not be called |
| 38 | `TestCSINode_NodeStage_Idempotent_StateFileExists` | Second NodeStageVolume when state file already exists and path is mounted is a no-op | First stage succeeded (state file written); second identical request; IsMounted→true | Returns empty NodeStageVolumeResponse; Connector.Connect called at most once |

---

### 5.10 Additional Input Validation (cross-cutting within CSI Node)

Input-validation gaps not covered in sections 5.2–5.4: empty required fields on
`NodeUnstageVolume`, `NodePublishVolume`, and `NodeUnpublishVolume`, and nil
`VolumeCapability` on staging.

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 39 | `TestCSINode_NodeStageVolume_MissingStagingTargetPath` | Missing staging_target_path returns InvalidArgument | StagingTargetPath="" in NodeStageVolumeRequest; valid VolumeID and context | Returns gRPC InvalidArgument |
| 40 | `TestCSINode_NodeStageVolume_NilVolumeCapability` | Nil volume_capability on NodeStageVolume returns InvalidArgument | VolumeCapability=nil; valid VolumeID, staging path, and context | Returns gRPC InvalidArgument |
| 41 | `TestCSINode_NodePublishVolume_MissingVolumeID` | Missing VolumeID on NodePublishVolume returns InvalidArgument | VolumeID="" in NodePublishVolumeRequest | Returns gRPC InvalidArgument |
| 42 | `TestCSINode_NodePublishVolume_MissingTargetPath` | Missing target_path on NodePublishVolume returns InvalidArgument | TargetPath=""; valid VolumeID and staging path | Returns gRPC InvalidArgument |
| 43 | `TestCSINode_NodeUnstageVolume_MissingVolumeID` | Missing VolumeID on NodeUnstageVolume returns InvalidArgument | VolumeID="" in NodeUnstageVolumeRequest | Returns gRPC InvalidArgument |
| 44 | `TestCSINode_NodeUnstageVolume_MissingStagingTargetPath` | Missing staging_target_path on NodeUnstageVolume returns InvalidArgument | StagingTargetPath=""; valid VolumeID | Returns gRPC InvalidArgument |
| 45 | `TestCSINode_NodeUnpublishVolume_MissingVolumeID` | Missing VolumeID on NodeUnpublishVolume returns InvalidArgument | VolumeID="" in NodeUnpublishVolumeRequest | Returns gRPC InvalidArgument |

---

### 5.11 Disconnect Error Paths (cross-cutting within CSI Node)

Error paths that occur during the NVMe-oF disconnect phase of
`NodeUnstageVolume`.

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 46 | `TestCSIErrors_NodeUnstage_DisconnectError` | NVMe-oF Disconnect failure during NodeUnstageVolume propagates | Stage volume; mock connector: Disconnect→"nvme disconnect failed"; call NodeUnstageVolume | Returns non-OK gRPC status; error message preserved |
| 47 | `TestCSIErrors_NodeUnstage_IsMountedError` | IsMounted check failure during NodeUnstageVolume propagates | Mock mounter: IsMounted→error; call NodeUnstageVolume | Returns non-OK gRPC status; no panic |

---

### 5.12 Concurrent Node Operations (cross-cutting within CSI Node)

Race and deadlock checks for concurrent node-level CSI calls.  Tests use
`t.TempDir()` state dirs and the mock Connector/Mounter; no real kernel
interactions needed.

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 48 | `TestCSINode_Concurrent_StageSameVolume_NoDeadlock` | Concurrent NodeStageVolume for the same VolumeID completes without deadlock | N goroutines simultaneously call NodeStageVolume with identical VolumeID; mock connector and mounter succeed | All goroutines complete within 5 s; no deadlock; no panic; final state: volume staged |
| 49 | `TestCSINode_Concurrent_StageDifferentVolumes_AllSucceed` | Concurrent NodeStageVolume for distinct VolumeIDs all succeed independently | N goroutines each staging a unique VolumeID; distinct staging paths | All goroutines complete without error; N separate state files created in stateDir |

---

### 5.13 IsMounted Error Paths (cross-cutting within CSI Node)

`Mounter.IsMounted` is called as an idempotency guard in `NodeStageVolume`
(before `FormatAndMount`), `NodePublishVolume` (before bind-mount), and
`NodeUnpublishVolume` (before `Unmount`).  These tests verify that a failure
of that check propagates correctly rather than leaving the node in an
indeterminate mount state.

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 50 | `TestCSIErrors_NodeStage_IsMountedError_MountAccess` | IsMounted failure during NodeStageVolume (mount access, after connect) returns Internal | Mock connector: Connect→OK, GetDevicePath→"/dev/nvme0n1"; mock mounter: IsMounted→error | Returns gRPC Internal; FormatAndMount not called |
| 51 | `TestCSIErrors_NodePublish_IsMountedError` | IsMounted failure during NodePublishVolume returns Internal | Mock mounter: IsMounted→error | Returns gRPC Internal; Mount not called |
| 52 | `TestCSIErrors_NodeUnpublish_IsMountedError` | IsMounted failure during NodeUnpublishVolume returns Internal | Volume published (setup); mock mounter: IsMounted→error on unpublish call | Returns gRPC Internal; Unmount not called |

---

## Test Infrastructure Notes

### Mock Interfaces Required

```go
// mockVolumeBackend (agent_test.go, agent_errors_test.go, concurrent_errors_test.go)
// Simplification: field-based; no command execution; mutex protects concurrent access.
type mockVolumeBackend struct {
    createDevicePath string; createAllocated int64; createErr error
    deleteErr error
    expandAllocated int64; expandErr error
    capacityTotal int64; capacityAvailable int64; capacityErr error
    listVolumesResult []*agentv1.VolumeInfo; listVolumesErr error
    devicePathResult string
}

// seqExec (zfs_test.go, zfs_errors_test.go)
// Simplification: strict sequential replay; no real process; no context awareness unless test arranges blocking.
type seqExec struct { responses []execResponse; pos int }

// mockAgentClient (csi_controller_test.go, csi_node_test.go, csi_errors_test.go)
// Simplification: in-process function fields; no gRPC transport; no retry.
type mockAgentClient struct {
    createVolumeFn   func(ctx, req) (*agentv1.CreateVolumeResponse, error)
    deleteVolumeFn   func(ctx, req) (*agentv1.DeleteVolumeResponse, error)
    exportVolumeFn   func(ctx, req) (*agentv1.ExportVolumeResponse, error)
    unexportVolumeFn func(ctx, req) (*agentv1.UnexportVolumeResponse, error)
    allowInitiatorFn func(ctx, req) (*agentv1.AllowInitiatorResponse, error)
    denyInitiatorFn  func(ctx, req) (*agentv1.DenyInitiatorResponse, error)
    expandVolumeFn   func(ctx, req) (*agentv1.ExpandVolumeResponse, error)
    // ... remaining RPCs
}

// mockConnector (csi_node_test.go, csi_errors_test.go)
// Simplification: no nvme-cli execution; fields determine return values.
type mockConnector struct {
    connectFn     func(ctx, nqn, addr string, port int32) error
    disconnectFn  func(ctx, nqn string) error
    getDevicePath func(ctx, nqn string) (string, error)
}

// mockMounter (csi_node_test.go, csi_errors_test.go)
// Simplification: no real mount syscall; IsMounted uses in-memory state.
type mockMounter struct {
    formatAndMountFn func(src, tgt, fs string, opts []string) error
    mountFn          func(src, tgt, fs string, opts []string) error
    unmountFn        func(tgt string) error
    isMountedFn      func(tgt string) (bool, error)
}
```

### Configfs Test Layout

Tests use `t.TempDir()` as `ConfigfsRoot`.  `NvmetTarget.Apply` writes to:
```
<tmpdir>/nvmet/subsystems/<nqn>/attr_allow_any_host
<tmpdir>/nvmet/subsystems/<nqn>/namespaces/1/device_path
<tmpdir>/nvmet/subsystems/<nqn>/namespaces/1/enable
<tmpdir>/nvmet/hosts/<nqn>/
<tmpdir>/nvmet/ports/<id>/addr_trtype
<tmpdir>/nvmet/ports/<id>/addr_traddr
<tmpdir>/nvmet/ports/<id>/addr_trsvcid
<tmpdir>/nvmet/ports/<id>/subsystems/<nqn>  (symlink)
```

### Test Execution Requirements

- **No root required**: All filesystem tests use `t.TempDir()` (auto-skips DAC tests if uid=0)
- **No real ZFS**: All ZFS operations use mock executor
- **No real kernel configfs**: `t.TempDir()` substitutes for `/sys/kernel/config`
- **No network**: Agent gRPC tests call handler methods directly; CSI tests use in-process mocks
- **Timeout**: All tests complete within 60 s total; device-poll tests use short timeouts (≤500 ms)
- **Parallelism**: All tests call `t.Parallel()` where safe

### Go Test File Organization

```
test/component/
├── TESTCASES.md                    # This document (authoritative spec)
├── agent_test.go                   # Component 1, sections 1.1–1.9
├── agent_errors_test.go            # Component 1, sections 1.10–1.13
├── concurrent_errors_test.go       # Component 1 §1.10, Component 3 §3.9
├── exceptions_test.go              # Component 1 §1.11–1.12, Component 2 §2.7, Component 3 §3.7–3.9
├── zfs_test.go                     # Component 2, sections 2.1–2.6
├── zfs_errors_test.go              # Component 2, sections 2.7–2.11
├── nvmeof_test.go                  # Component 3, sections 3.1–3.6
├── nvmeof_errors_test.go           # Component 3, sections 3.7–3.10
├── csi_controller_test.go          # Component 4, sections 4.1–4.5
├── csi_controller_extended_test.go # Component 4, sections 4.6–4.9
├── csi_errors_test.go              # Component 4 §4.6, Component 5 §5.6, §5.11
├── csi_node_test.go                # Component 5, sections 5.1–5.7, §5.9–5.10
├── csi_node_extended_test.go       # Component 5, sections 5.8, §5.12
└── helpers_test.go                 # Shared test helpers and mock implementations
```

---

## Component 6: CSI Identity Service (`internal/csi/`)

**Subsystem-boundary setup:** `csi.IdentityServer` constructed with a driver
name, driver version, and an injectable `readyFn`.  All three RPC methods
(GetPluginInfo, GetPluginCapabilities, Probe) are called directly in-process
with no network transport.  Error paths (context cancellation, readiness
failure, health-check error) are tested here because they exercise the identity
server's error-handling logic.

### mockIdentityReadyFn (used by CSI Identity tests)
```go
// mockIdentityReadyFn is a field-based test double for the readyFn parameter
// of NewIdentityServerWithReadyFn.
// Simplifications vs. a real health check:
//   - No I/O; result is a preset field (ready bool, err error).
//   - Does not inspect the context unless the test sets blockUntilCtxDone=true.
//   - Blocking variant uses ctx.Done() to simulate a slow readiness check.
```

---

### 6.1 GetPluginInfo

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 1 | `TestCSIIdentity_GetPluginInfo_Success` | Returns driver name and version | IdentityServer with name="pillar-csi.bhyoo.com", version="0.1.0" | Response.Name="pillar-csi.bhyoo.com"; Response.VendorVersion="0.1.0" |
| 2 | `TestCSIIdentity_GetPluginInfo_NameNotEmpty` | Driver name is never empty | IdentityServer constructed with valid name | Response.Name != "" |

---

### 6.2 GetPluginCapabilities

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 3 | `TestCSIIdentity_GetPluginCapabilities_IncludesControllerService` | Returns CONTROLLER_SERVICE capability | IdentityServer constructed | At least one capability with Service.Type=CONTROLLER_SERVICE |
| 4 | `TestCSIIdentity_GetPluginCapabilities_IncludesVolumeExpansion` | Returns VOLUME_EXPANSION capability | IdentityServer constructed | At least one capability with VolumeExpansion type |

---

### 6.3 Probe

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 5 | `TestCSIIdentity_Probe_Ready` | Probe returns Ready=true when readyFn returns (true, nil) | readyFn: (true, nil) | Response.Ready.Value=true; no error |
| 6 | `TestCSIIdentity_Probe_NotReady` | Probe returns Ready=false when readyFn returns (false, nil) | readyFn: (false, nil) | Response.Ready.Value=false; no error |
| 7 | `TestCSIIdentity_Probe_DefaultAlwaysReady` | Default IdentityServer (no readyFn) always returns Ready=true | NewIdentityServer (no readyFn) | Response.Ready.Value=true; no error |

---

### 6.4 Error Paths (cross-cutting within CSI Identity)

| # | Test Function | Description | Setup | Expected Outcome |
|---|--------------|-------------|-------|-----------------|
| 8 | `TestCSIIdentity_GetPluginInfo_ContextDeadlineExceeded` | GetPluginInfo with already-expired context returns DeadlineExceeded | context.WithTimeout→expired before call; call GetPluginInfo | Returns gRPC DeadlineExceeded |
| 9 | `TestCSIIdentity_GetPluginCapabilities_ContextCancelled` | GetPluginCapabilities with already-cancelled context returns Cancelled | context.WithCancel→cancel(); call GetPluginCapabilities | Returns gRPC Cancelled |
| 10 | `TestCSIIdentity_Probe_ContextDeadlineExceeded` | Probe with already-expired context returns DeadlineExceeded before calling readyFn | context.WithTimeout→expired; readyFn never called | Returns gRPC DeadlineExceeded; readyFn call count = 0 |
| 11 | `TestCSIIdentity_Probe_ReadyFnError_ReturnsInternal` | Probe returns Internal when readyFn returns a non-context error | readyFn: error("health check failed: disk quota exceeded") | Returns gRPC Internal; error message contains health-check detail |
| 12 | `TestCSIIdentity_Probe_ReadyFnContextError_PropagatesCode` | Probe returns DeadlineExceeded when readyFn propagates context error | readyFn: returns context.DeadlineExceeded | Returns gRPC DeadlineExceeded (not Internal) |

---

## Test Infrastructure Notes (continued)

### Go Test File Organization (updated)

```
test/component/
├── TESTCASES.md                    # This document (authoritative spec)
├── agent_test.go                   # Component 1, sections 1.1–1.9
├── agent_errors_test.go            # Component 1, sections 1.10–1.13
├── concurrent_errors_test.go       # Component 1 §1.10, Component 3 §3.9
├── exceptions_test.go              # Component 1 §1.11–1.12, Component 2 §2.7, Component 3 §3.7–3.9
├── zfs_test.go                     # Component 2, sections 2.1–2.6
├── zfs_errors_test.go              # Component 2, sections 2.7–2.11
├── nvmeof_test.go                  # Component 3, sections 3.1–3.6
├── nvmeof_errors_test.go           # Component 3, sections 3.7–3.10
├── csi_controller_test.go          # Component 4, sections 4.1–4.5
├── csi_controller_extended_test.go # Component 4, sections 4.6–4.9
├── csi_errors_test.go              # Component 4 §4.6, Component 5 §5.6, §5.11
├── csi_node_test.go                # Component 5, sections 5.1–5.7, §5.9–5.10
├── csi_node_extended_test.go       # Component 5, sections 5.8, §5.12
├── csi_identity_test.go            # Component 6, sections 6.1–6.4
└── helpers_test.go                 # Shared test helpers and mock implementations
```

---

## Summary: Test Count by Component

| Component | Sections | Test Count |
|-----------|----------|-----------|
| 1. Agent gRPC Server | 1.1–1.13 | 65 |
| 2. ZFS Backend | 2.1–2.11 | 37 |
| 3. NVMe-oF configfs Target | 3.1–3.15 | 65 |
| 4. CSI Controller Service | 4.1–4.9 | 50 |
| 5. CSI Node Service | 5.1–5.13 | 52 |
| 6. CSI Identity Service | 6.1–6.4 | 12 |
| **Total** | | **281** |

*(Counts include all rows in all tables.  Cross-cutting exception paths for each
component are embedded in the relevant component section, not in a separate
pseudo-component.  Actual implementations may include a small number of
additional helper tests not counted above; see `go test -v` output for the
authoritative list.)*

---

## Coverage Validation: Error Path Categories

This section documents that the implementation satisfies the following coverage
requirements:

- **≥ 15 distinct error paths** implemented across **≥ 4 components**
- **All 8 required error categories** are represented

The eight required categories and representative implementations are listed
below.  Each cell identifies the component (1–6) and the test function that
exercises that category within that component.

---

### Category 1 — Invalid Argument / Validation Errors (`codes.InvalidArgument`)

Malformed or missing request fields that must be rejected before any backend
call is made.

| Component | Test Function | Error Condition |
|-----------|--------------|----------------|
| 1 – Agent | `TestAgentServer_CreateVolume_InvalidVolumeID` | VolumeID with no pool separator |
| 1 – Agent | `TestAgentErrors_CreateVolume_EmptyVolumeID` | VolumeID="" on CreateVolume |
| 1 – Agent | `TestAgentErrors_DeleteVolume_EmptyVolumeID` | VolumeID="" on DeleteVolume |
| 1 – Agent | `TestAgentErrors_ExpandVolume_EmptyVolumeID` | VolumeID="" on ExpandVolume |
| 1 – Agent | `TestAgentErrors_ExportVolume_MissingNvmeofTcpParams` | NVMEOF_TCP protocol with nil params |
| 4 – CSI Controller | `TestCSIController_CreateVolume_MissingName` | Empty volume name |
| 4 – CSI Controller | `TestCSIController_CreateVolume_MissingParams` | Missing required storage class params |
| 4 – CSI Controller | `TestCSIErrors_CreateVolume_InvalidCapabilities_Nil` | Nil VolumeCapabilities |
| 4 – CSI Controller | `TestCSIErrors_ControllerExpand_InvalidArgument_EmptyVolumeID` | VolumeID="" on ControllerExpandVolume |
| 5 – CSI Node | `TestCSIErrors_NodeStage_InvalidParams_MissingVolumeID` | VolumeID="" on NodeStageVolume |
| 5 – CSI Node | `TestCSINode_NodeStageVolume_MissingStagingTargetPath` | Empty staging_target_path |

**Count: 11 error paths across 3 components** ✓

---

### Category 2 — Not Found (`codes.NotFound`)

Requests referencing a resource that does not exist.

| Component | Test Function | Error Condition |
|-----------|--------------|----------------|
| 1 – Agent | `TestAgentServer_CreateVolume_InvalidPool` | Pool name not registered |
| 1 – Agent | `TestAgentServer_DeleteVolume_InvalidPool` | Pool name not registered |
| 1 – Agent | `TestAgentServer_ExpandVolume_InvalidPool` | Pool name not registered |
| 1 – Agent | `TestAgentServer_GetCapacity_UnknownPool` | Pool not in backend registry |
| 4 – CSI Controller | `TestCSIController_CreateVolume_TargetNotFound` | PillarTarget CR missing |
| 4 – CSI Controller | `TestCSIController_ControllerPublishVolume_TargetNotFound` | PillarTarget CR missing |
| 4 – CSI Controller | `TestCSIController_ExpandVolume_TargetNotFound` | PillarTarget CR missing |

**Count: 7 error paths across 2 components** ✓

---

### Category 3 — Already Exists / Conflict (`codes.AlreadyExists`)

Requests that conflict with an already-existing resource.

| Component | Test Function | Error Condition |
|-----------|--------------|----------------|
| 1 – Agent | `TestAgentServer_CreateVolume_ConflictSize` | Existing volume with different capacity |
| 2 – ZFS | `TestZFSBackend_Create_ConflictDifferentSize` | Existing zvol with different size |
| 4 – CSI Controller | `TestCSIController_CreateVolume_DuplicateName` | Same name, conflicting capacity |

**Count: 3 error paths across 3 components** ✓

---

### Category 4 — Resource Exhaustion (`codes.ResourceExhausted`)

Operations that fail because a resource limit has been reached.

| Component | Test Function | Error Condition |
|-----------|--------------|----------------|
| 1 – Agent | `TestAgentServer_CreateVolume_DiskFull` | Backend reports out-of-space |
| 1 – Agent | `TestAgentErrors_CreateVolume_DiskFullPropagation` | Disk-full detail preserved through gRPC layer |
| 2 – ZFS | `TestZFSBackend_Create_DiskFull` | `zfs create` returns ENOSPC |
| 2 – ZFS | `TestZFSBackend_Error_DiskFull_Expand` | `zfs set volsize` returns ENOSPC |

**Count: 4 error paths across 2 components** ✓

---

### Category 5 — Permission Denied (POSIX `EPERM`/`EACCES`)

Operations that fail due to missing filesystem or device permissions.

| Component | Test Function | Error Condition |
|-----------|--------------|----------------|
| 1 – Agent | `TestAgentServer_ExportVolume_PermissionError` | `os.ErrPermission` from device checker |
| 3 – NVMe-oF | `TestException_ConfigfsWritePermissionDenied` | nvmet/subsystems/ dir is read-only (0555) |
| 3 – NVMe-oF | `TestNvmeof_Error_Apply_PortsDirPermissionDenied` | nvmet/ports/ dir is read-only (0555) |
| 3 – NVMe-oF | `TestNvmeof_Error_AllowHost_HostsDirPermissionDenied` | hosts/ dir is read-only (0444) |
| 3 – NVMe-oF | `TestNvmeof_Error_AllowHost_AllowedHostsDirPermissionDenied` | allowed_hosts/ dir is read-only (0444) |
| 3 – NVMe-oF | `TestNvmeof_Error_Apply_EnableFileReadOnly` | namespace enable file is read-only (0444) |

**Count: 6 error paths across 2 components** ✓

---

### Category 6 — Context Cancellation (`codes.Canceled`)

Operations cancelled by the caller via `context.Cancel`.

| Component | Test Function | Error Condition |
|-----------|--------------|----------------|
| 1 – Agent | `TestAgentErrors_ExportVolume_ContextCancelledDuringPoll` | Context cancelled during device poll loop |
| 2 – ZFS | `TestZFSBackend_Error_ContextCancelled_Delete` | Context cancelled before `zfs destroy` |
| 2 – ZFS | `TestZFSBackend_Error_ContextCancelled_Expand` | Context cancelled before `zfs set volsize` |
| 2 – ZFS | `TestZFSBackend_Error_ContextCancelled_ListVolumes` | Context cancelled before `zfs list` |
| 2 – ZFS | `TestZFSBackend_Error_ContextCancelled_Capacity` | Context cancelled before `zpool list` |
| 6 – CSI Identity | `TestCSIIdentity_GetPluginCapabilities_ContextCancelled` | Already-cancelled context on GetPluginCapabilities |

**Count: 6 error paths across 3 components** ✓

---

### Category 7 — Deadline Exceeded / Timeout (`codes.DeadlineExceeded`)

Operations that fail because a time budget was exhausted.

| Component | Test Function | Error Condition |
|-----------|--------------|----------------|
| 1 – Agent | `TestAgentServer_ExportVolume_DeviceNotReady` | Device never appears within poll timeout |
| 1 – Agent | `TestException_GRPCDeadlineExceeded` | gRPC request deadline propagated to backend |
| 2 – ZFS | `TestException_ZFSCommandTimeout` | ZFS command blocks until context deadline fires |
| 3 – NVMe-oF | `TestNvmeof_DevicePoll_NeverAppears` | Device checker always returns false; short ctx deadline |
| 3 – NVMe-oF | `TestNvmeof_Error_WaitForDevice_ShortTimeoutNeverAppears` | Short poll timeout with no device appearance |
| 4 – CSI Controller | `TestCSIErrors_CreateVolume_AgentDeadlineExceeded` | Agent returns DeadlineExceeded on CreateVolume |
| 4 – CSI Controller | `TestCSIErrors_ControllerExpand_AgentDeadlineExceeded` | Agent returns DeadlineExceeded on ExpandVolume |
| 4 – CSI Controller | `TestCSIErrors_DeleteVolume_AgentDeadlineExceeded` | Agent returns DeadlineExceeded on DeleteVolume |
| 5 – CSI Node | `TestCSIErrors_NodeStage_GRPCDeadlineExceeded` | gRPC deadline during NodeStageVolume |
| 6 – CSI Identity | `TestCSIIdentity_GetPluginInfo_ContextDeadlineExceeded` | Already-expired context on GetPluginInfo |
| 6 – CSI Identity | `TestCSIIdentity_Probe_ContextDeadlineExceeded` | Already-expired context short-circuits readyFn |
| 6 – CSI Identity | `TestCSIIdentity_Probe_ReadyFnContextError_PropagatesCode` | readyFn blocks until deadline; code propagated |

**Count: 12 error paths across 5 components** ✓

---

### Category 8 — TOCTOU, Partial Failure, and Concurrency

Race conditions and partial-write failures that arise from concurrent or
multi-step operations.

| Component | Test Function | Error Condition |
|-----------|--------------|----------------|
| 1 – Agent | `TestAgentErrors_ExportVolume_ConfigfsBrokenAfterDeviceCheck_TOCTOU` | configfs becomes read-only after device check passes |
| 1 – Agent | `TestAgentServer_ConcurrentExportUnexport` | Concurrent Export + Unexport same volume (no deadlock) |
| 1 – Agent | `TestConcurrentError_CreateVolume_SameID_NoDeadlock` | N goroutines CreateVolume same VolumeID simultaneously |
| 1 – Agent | `TestException_ConcurrentExportUnexport` | Concurrent re-export + unexport with real configfs state |
| 2 – ZFS | `TestZFSBackend_Error_CreateReadback_DatasetGone` | Dataset disappears between create and size readback |
| 2 – ZFS | `TestZFSBackend_Error_ExpandReadback_PoolFailed` | Pool fails between volsize set and readback |
| 3 – NVMe-oF | `TestNvmeof_Apply_PartialFailureMidApply` | Port dir creation blocked; subsystem partially written |
| 3 – NVMe-oF | `TestConcurrentError_Apply_AttrAllowAnyHostReadOnly` | attr_allow_any_host file read-only mid-apply |
| 3 – NVMe-oF | `TestConcurrentError_AllowDenyInitiator_SameHost_Race` | Concurrent AllowHost + DenyHost for same NQN |
| 3 – NVMe-oF | `TestException_ConfigfsDirCreateRace` | Concurrent Apply goroutines sharing same port dir |
| 4 – CSI Controller | `TestCSIController_CreateVolume_ExportFails_RecordsCreatePartial` | ExportVolume fails after backend create (partial CRD state) |
| 5 – CSI Node | `TestCSIErrors_NodeStage_TOCTOU_DeviceDisappears` | Device path disappears between GetDevicePath and FormatAndMount |
| 5 – CSI Node | `TestCSINode_Concurrent_StageSameVolume_NoDeadlock` | Concurrent NodeStageVolume for same VolumeID |

**Count: 13 error paths across 5 components** ✓

---

### Coverage Summary

| Requirement | Required | Actual | Status |
|-------------|----------|--------|--------|
| Distinct error paths | ≥ 15 | **62** (across categories 1–8) | ✓ PASS |
| Components covered | ≥ 4 | **6** (Components 1–6) | ✓ PASS |
| Error categories covered | 8 | **8** (categories 1–8 above) | ✓ PASS |

All three coverage requirements are satisfied.  Error paths are distributed
across all six components, with each component contributing tests in at least
two error categories.  The CSI Identity Service (Component 6, the
driver-registrar equivalent) contributes error paths in Category 6
(context cancellation), Category 7 (deadline exceeded), and the generic
health-check failure path that maps to `codes.Internal`.
