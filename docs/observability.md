# Monitoring and tracing

pillar-csi exposes Prometheus metrics from the controller, the storage agent and the node plugin, and it can send OpenTelemetry traces over OTLP/gRPC. Metrics tell you that something is wrong across the fleet. Traces tell you which step failed for one volume. Failure log lines carry a `trace_id` that links the two.

- [Enable metrics and tracing](#enable-metrics-and-tracing)
- [Security notes](#security-notes)
- [Ports](#ports)
- [Which signal answers my question?](#which-signal-answers-my-question)
- [Metric reference](#metric-reference)
- [Trace reference](#trace-reference)
- [Find the traces of a PVC, PV or Pod](#find-the-traces-of-a-pvc-pv-or-pod)
- [Go from a failure log line to its trace](#go-from-a-failure-log-line-to-its-trace)
- [Did an agent call fail in transport or in the agent?](#did-an-agent-call-fail-in-transport-or-in-the-agent)
- [Example alert rules](#example-alert-rules)
- [Known limitation: mTLS breaks the CSI to agent path](#known-limitation-mtls-breaks-the-csi-to-agent-path)

## Enable metrics and tracing

Everything is off by default except the controller's own metrics endpoint.

```sh
helm upgrade --install pillar-csi oci://ghcr.io/isac322/charts/pillar-csi \
  --namespace pillar-csi \
  -f values.yaml \
  --set metrics.enabled=true \
  --set metrics.podMonitor.enabled=true \
  --set tracing.enabled=true \
  --set tracing.endpoint=http://tempo-distributor.monitoring:4317
```

| value | default | effect |
|---|---|---|
| `metrics.enabled` | `false` | Serves `/metrics` on the agent (port `metrics.agent.port`, 9501) and the node plugin (`metrics.node.port`, 9502), and turns on the `--http-endpoint` of the CSI provisioner (8090), attacher (8091) and resizer (8092) sidecars. |
| `metrics.controller.secure` | `true` | The controller endpoint (`controller.metricsPort`, 8080) is always served. `true` serves it over HTTPS and checks every scrape with a TokenReview and a SubjectAccessReview. `false` serves plain HTTP with no auth. |
| `metrics.podMonitor.enabled` | `false` | Renders prometheus-operator PodMonitors. Needs the `monitoring.coreos.com/v1` PodMonitor CRD; the render fails with a clear message without it. `interval`, `scrapeTimeout` and `labels` apply to every PodMonitor. |
| `tracing.enabled` | `false` | Renders the `OTEL_*` environment into the controller, agent and node. The render fails if `tracing.endpoint` is empty. |
| `tracing.endpoint` | `""` | OTLP/gRPC endpoint, for example `http://tempo-distributor.monitoring:4317`, or `http://$(HOST_IP):4317` for a collector on each node. `HOST_IP` is set in the controller, agent and node containers. |
| `tracing.insecure` | `true` | Sets `OTEL_EXPORTER_OTLP_INSECURE`. |
| `tracing.samplerRatio` | `"1.0"` | Sets `OTEL_TRACES_SAMPLER_ARG`: the fraction of new traces to keep, from 0 to 1. A value that doesn't parse stops the binary at startup. |

What the PodMonitors scrape:

| PodMonitor | endpoints | rendered when |
|---|---|---|
| `<fullname>-controller` | `metrics`: HTTPS with a bearer token from the `<fullname>-metrics-reader-token` Secret and `insecureSkipVerify: true` (the certificate is self-signed unless `--metrics-cert-path` is set). With `metrics.controller.secure=false`: plain HTTP, no auth. | `metrics.podMonitor.enabled` |
| `<fullname>-controller` | `prov-metrics` (8090), `attach-metrics` (8091), `resize-metrics` (8092): the CSI sidecars | also `metrics.enabled` |
| `<fullname>-agent`, `<fullname>-node` | `metrics` | also `metrics.enabled` |

When the controller endpoint is secure, PodMonitors are on and `rbac.create` is true, the chart also creates a `<fullname>-metrics-reader` ClusterRole (`get` on `/metrics`), ServiceAccount, token Secret and ClusterRoleBinding. To scrape the controller without prometheus-operator, give your Prometheus a service account token bound to the same kind of ClusterRole.

Tracing details:

- A binary exports traces only when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set. Only OTLP over gRPC is supported; any other `OTEL_EXPORTER_OTLP_PROTOCOL` stops the binary at startup.
- Each process reports `service.name` = `pillar-csi-controller`, `pillar-csi-agent` or `pillar-csi-node`, `service.version`, and `k8s.namespace.name`, `k8s.pod.name`, `k8s.node.name`.
- Use the component's `extraEnv` for other standard variables, such as `OTEL_EXPORTER_OTLP_HEADERS`. `extraEnv` renders last, so it overrides the chart's values.
- The agent and node run with `hostNetwork`. They keep cluster DNS, so a Service name works as the endpoint.

To see `trace_id` exemplars on agent and node histograms, run Prometheus with `--enable-feature=exemplar-storage`. The agent and node serve OpenMetrics; the controller endpoint does not, so controller metrics carry no exemplars.

## Security notes

- **Agent and node metrics are plaintext HTTP with no auth.** They bind to the host IP (`[$(HOST_IP)]:9501` and `:9502`), not `0.0.0.0`, so they stay off other host interfaces, but anyone who can reach the node IP can read them. Metrics carry no volume IDs, PV names or NQNs. They do carry pool names, agent names and NVMe-oF target addresses. Restrict access with a NetworkPolicy or host firewall if that matters to you.
- **The agent accepts `traceparent` from any caller.** Its gRPC port is plaintext by default, so a client can send `traceparent` with the sampled flag set and force the agent to record and export spans. That client could already call any agent RPC, so this adds no new access, but it can add load on your trace backend.
- Spans never record configfs values, the NVMe-oF connect option string, or `mkfs`/resize arguments. On a failed `zfs`, `lvs`, `vgs`, `lvcreate`, `lvremove`, `lvextend` or `dmsetup` command, the span records the full argv and the last 1 KiB of output.
- Failure log lines repeat the gRPC status message, which the PVC or Pod Event already shows.

## Ports

| component | network | existing ports | new ports |
|---|---|---|---|
| agent | hostNetwork | gRPC 9500 | metrics 9501 |
| node | hostNetwork | liveness 9808 | metrics 9502 |
| controller | pod network | health 8081, metrics 8080, webhook 9443, liveness 9809 | sidecar metrics 8090 (provisioner), 8091 (attacher), 8092 (resizer) |

9501 and 9502 avoid the usual node-exporter (9100), kubelet (10250) and kube-proxy (10249, 10256) ports. Change `metrics.agent.port` or `metrics.node.port` if something else on your hosts uses them.

## Which signal answers my question?

| question | look at |
|---|---|
| Is a pool about to fill up (including thin-pool metadata), and when? | `pillar_csi_pool_available_bytes`, `pillar_csi_pool_size_bytes`, `pillar_csi_pool_thin_metadata_used_ratio`, joined to stores with `pillar_csi_store_info` |
| How overcommitted are my thin pools? | `pillar_csi_pool_provisioned_bytes` / `pillar_csi_pool_size_bytes` |
| Are all agents reachable and healthy? Which subsystem is broken? | `pillar_csi_resource_status_condition{kind="PillarAgent"}`, `pillar_csi_agent_subsystem_healthy`, `pillar_csi_agent_client_requests_total` |
| Are volume operations failing now? Which operation, which agent, which error? | `pillar_csi_agent_client_requests_total`, `grpc_server_handled_total`, `csi_sidecar_operations_seconds` |
| Are operations within the latency SLO, and which component is slow? | `csi_sidecar_operations_seconds`, kubelet `csi_operations_seconds`, `grpc_server_handling_seconds`, `pillar_csi_exec_duration_seconds` |
| Why is *this* PVC Pending, *this* Pod ContainerCreating, *this* expansion or deletion stuck? | [Traces of the PVC](#find-the-traces-of-a-pvc-pv-or-pod) and the [failure log line](#go-from-a-failure-log-line-to-its-trace) |
| How long did each step of my volume's provisioning and attach take? | Traces: SP1, SP5, SP6, SP8 to SP13 |
| Was my volume attached locally or over NVMe-oF, which storage node serves it, is it read-only on purpose? | Traces: `pillar_csi.attach_mode`, `pillar_csi.agent.name`, `pillar_csi.readonly` on SP1 and SP9 |
| Did pillar-csi format my volume on this stage? | Trace SP13 `pillar_csi.mkfs.performed`; fleet-wide `pillar_csi_exec_duration_seconds{executable=~"mkfs.*"}` |
| Is worker X's NVMe-oF path to storage node Y degraded? | `pillar_csi_node_nvmeof_controllers` |
| Are volumes stuck half-created, or exports unreconciled or unrestored after an agent restart? | `pillar_csi_volumes`, `pillar_csi_volumes_export_unreconciled`, `pillar_csi_resource_status_condition{type="ExportsReady"}`, trace SP3 |
| When do the certificates loaded in memory expire? | `pillar_csi_tls_certificate_not_after_timestamp_seconds` |
| Is the controller leader-elected and healthy? | controller-runtime: `leader_election_master_status`, `controller_runtime_reconcile_*`, `workqueue_*`, `rest_client_requests_total` |
| Is an agent overloaded or hung? | `grpc_server_started_total` minus `grpc_server_handled_total`, `pillar_csi_exec_duration_seconds`, `process_*`, `go_*` |
| Which version runs where? | `pillar_csi_build_info`; resource `service.version` on traces |
| Is fencing rejecting operations, and why? | `pillar_csi_agent_fencing_decisions_total`; SP5 `pillar_csi.fence.decision` |
| Did pillar-csi delete backend storage on its own? | `pillar_csi_volumes_reaped_total`, trace SP4 |
| Which external command failed, with what exit code and output? | Trace SP8; fleet-wide `pillar_csi_exec_duration_seconds{result!="ok"}` |
| Which configfs write failed, with which errno? | Trace SP7; fleet-wide `pillar_csi_nvmet_configfs_errors_total` |
| Did a controller→agent call fail in transport or in the agent? | [Transport vs agent](#did-an-agent-call-fail-in-transport-or-in-the-agent) |
| How long did an agent call wait on the per-target lock? | SP5 `pillar_csi.lock.target.wait_duration` |
| Did PillarVolumeState updates happen in order? Was this call a retry? | SP1 `pillar_csi.pvs.update` events, `pillar_csi.create.resumed_from` |
| Did an export-restore batch succeed, and which volumes failed in it? | SP3 and the SP5 `ReconcileState` span with its `pillar_csi.reconcile.item_failed` events |
| How long did the agent wait for the zvol or LV device? | SP6 |
| Is a PVC's filesystem nearly full? | kubelet `kubelet_volume_stats_*` |
| Is the fencing-mark fsync slowing every mutating RPC? | node-exporter `node_disk_*` on the storage node; the effect shows in `grpc_server_handling_seconds` |

## Metric reference

Rules that hold for every `pillar_csi_*` metric:

- Base units: seconds, bytes, ratios from 0 to 1. Counters end in `_total`.
- **No per-volume labels.** No metric has a volume ID, PV, PVC, NQN or device path label. Use traces for one volume.
- Every label has a fixed set of values. Input the code doesn't recognize is reported as `other`.
- The component (controller, agent or node) comes from the scrape target labels (`job`, `pod`, `instance`). Only `pillar_csi_build_info` has a `component` label.
- Metrics marked **leader only** come from the elected controller replica. Other replicas emit none, so sums don't double count.

| id | name | type | labels and allowed values | unit | from | answers |
|---|---|---|---|---|---|---|
| M1 | `pillar_csi_pool_size_bytes` | gauge | `pool` (pool or VG name from the agent config); `backend`: `zfs-zvol`, `lvm-lv` | bytes | agent | How big is each pool? |
| M2 | `pillar_csi_pool_available_bytes` | gauge | `pool`, `backend` | bytes | agent | How much space can new volumes still use? This is the same refreservation-aware value that CreateVolume checks. |
| M3 | `pillar_csi_pool_provisioned_bytes` | gauge | `pool`; `backend`: `zfs-zvol`, `lvm-lv` (LVM thin pools only) | bytes | agent | How much have I promised? ZFS: sum of `volsize` under the parent dataset. LVM thin: sum of LV sizes in the thin pool. Not emitted for linear LVM, which cannot overcommit. Refreshed every 5 minutes. |
| M4 | `pillar_csi_pool_thin_metadata_used_ratio` | gauge | `pool`; `backend`: `lvm-lv` | ratio | agent | Is the LVM thin-pool metadata filling up? LVM thin only. |
| M5 | `pillar_csi_agent_subsystem_healthy` | gauge (0 or 1) | `subsystem`: `nvmet_configfs`, `pool`; `pool` (`""` for `nvmet_configfs`, else the pool name) | - | agent | Which part of the agent is broken: the nvmet configfs or a specific pool? 1 = healthy. |
| M6 | `grpc_server_started_total`, `grpc_server_handled_total`, `grpc_server_handling_seconds` | counter, counter, histogram | `grpc_type`: `unary`, `server_stream`, `client_stream`, `bidi_stream`; `grpc_service`: `pillar_csi.agent.v1.AgentService`, `grpc.health.v1.Health`; `grpc_method`: the 17 AgentService RPCs plus `Check`, `Watch`; `grpc_code` (counters only): the 17 gRPC code names | seconds; buckets 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120 | agent | What does the agent itself see: calls, failures by code, and handling time? Rejections by drain and by the export-restore gate are counted too. `started - handled` = calls in flight. |
| M7 | `pillar_csi_agent_fencing_decisions_total` | counter | `fence_op`: `grant`, `revoke`, `destroy`; `decision`: see [below](#fencing-decisions) | - | agent | Is fencing rejecting operations, for which reason, and is it spiking? |
| M8 | `pillar_csi_nvmet_configfs_errors_total` | counter | `op`: `write`, `trigger`, `mkdir`, `symlink`, `unlink`, `rmdir`; `errno`: `EBUSY`, `EINVAL`, `ENOENT`, `EEXIST`, `EPERM`, `EACCES`, `ENOSPC`, `ENODEV`, `other` | - | agent | Which configfs operation fails, with which errno, and is one errno spiking across agents? Counts only errors returned to the caller; tolerated best-effort teardown misses (EPERM rmdir of kernel default groups, ENOENT disable write on an already-removed namespace) are not counted. |
| M9 | `pillar_csi_exec_duration_seconds` | histogram | `executable`: `zfs`, `lvs`, `vgs`, `lvcreate`, `lvremove`, `lvextend`, `dmsetup`, `mkfs.ext4`, `mkfs.ext3`, `mkfs.ext2`, `mkfs.xfs`, `resize2fs`, `xfs_growfs`, `other`; `subcommand`: for `zfs` `create`, `destroy`, `set`, `get`, `list`; for `dmsetup` `create`, `reload`, `resume`, `suspend`, `remove`, `table`, `info`, `status`; unknown `other`; `""` for other executables; `result`: `ok`, `exit_error`, `start_error`, `canceled` | seconds; buckets 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300 | agent, node | How long do external commands take, and is one failing fleet-wide? Every command is counted, traced or not. |
| M10 | `pillar_csi_agent_client_requests_total` | counter | `agent` (PillarAgent name; the dial target if unknown); `method`: the 17 AgentService RPC names; `code`: the 17 gRPC code names | - | controller | What does the controller see when it calls each agent? Compare with M6 to tell transport failures from agent failures. |
| M11 | `pillar_csi_resource_status_condition` | gauge (0 or 1) | `kind`: `PillarAgent`, `PillarStore`; `name`; `type` (condition type); `status`: `True`, `False`, `Unknown`; `reason` | - | controller, leader only | Which agents and stores are not ready, and why? One series per object and condition; the current status has value 1. |
| M12 | `pillar_csi_store_info` | gauge (always 1) | `store`, `agent`, `pool`, `backend`: `zfs-zvol`, `lvm-lv` | - | controller, leader only | Which store is this pool? Join agent pool metrics to store names on `pool`. |
| M13 | `pillar_csi_volumes` | gauge | `agent`, `backend`, `phase`: `Provisioning`, `CreatePartial`, `Ready`, `Deleting` | volumes | controller, leader only | How many volumes each agent has, and are any stuck half-created? |
| M14 | `pillar_csi_volumes_export_unreconciled` | gauge | `agent`; `reason`: `ExportSpecMissing`, `AgentUnavailable`, `ReconcileFailed`, `StaleGeneration` | volumes | controller, leader only | How many volumes have a storage-side export that doesn't match what it should be, and why? |
| M15 | `pillar_csi_volumes_reaped_total` | counter | `agent`; `result`: `reaped`, `kept`, `error` | - | controller | Did pillar-csi delete backend storage of an abandoned provisioning on its own, and did that fail? |
| M16 | `pillar_csi_tls_certificate_not_after_timestamp_seconds` | gauge | `role`: `agent_server`, `controller_client`, `ca` | seconds (Unix time) | agent, controller | When do the certificates **loaded in memory** expire? They are read once at startup, so a rotated Secret doesn't change this until a restart. Absent without mTLS. |
| M17 | `pillar_csi_node_nvmeof_controllers` | gauge | `target_address` (`traddr:trsvcid`); `state`: `live`, `connecting`, `resetting`, `deleting`, `deleting (no IO)`, `new`, `dead`, `other` | controllers | node | Is this worker's NVMe-oF path to a storage node degraded right now? Counts only pillar-csi subsystems. |
| M18 | `pillar_csi_build_info` | gauge (always 1) | `component`: `controller`, `agent`, `node`; `version`, `revision`, `go_version` | - | all | Which version and revision runs where? |
| M19 | `go_*`, `process_*` | library default | library default | - | agent, node | Is the process short of memory, CPU or file descriptors? The controller already exports these through controller-runtime. |

Pool metrics (M1, M2, M4, M5) come from a cache that refreshes at most every 15 seconds with a 10 second timeout. If a pool's refresh fails or times out, its series disappear until the next good refresh. A scrape never runs a storage command itself.

### Fencing decisions

`decision` values of `pillar_csi_agent_fencing_decisions_total` (and of the span attribute `pillar_csi.fence.decision`):

| value | meaning |
|---|---|
| `admit_new_lifecycle` | First operation for this volume, or a new lifecycle after the previous one ended. |
| `admit_advance` | The token advanced the stored generation. |
| `admit_same_generation` | The token matches the stored generation. |
| `admit_terminal_retry` | A retry of the same destroy after the lifecycle ended. |
| `reject_missing_token` | The request carried no fencing token. |
| `reject_retired` | The token belongs to a retired lifecycle. |
| `reject_other_owner` | Another owner holds the volume. |
| `reject_ended` | The lifecycle already ended. |
| `reject_superseded` | A newer generation superseded the token. |
| `reject_mark_changed` | The fencing mark changed while the operation ran. |
| `mark_io_error` | Reading or writing the fencing mark on disk failed. |

### Existing metrics pillar-csi relies on

The chart makes these scrapable; pillar-csi adds nothing to them.

- CSI sidecars (with `metrics.enabled`): `csi_sidecar_operations_seconds{driver_name="pillar-csi.bhyoo.com", method_name, grpc_status_code}` from the provisioner, attacher and resizer. Per-CSI-method totals and latency as Kubernetes sees them.
- kubelet: `csi_operations_seconds` (node-side CSI latency) and `kubelet_volume_stats_*` (filesystem usage of each PVC).
- controller-runtime (controller endpoint): `leader_election_master_status{name="bf3a431e.pillar-csi.bhyoo.com"}`, `controller_runtime_reconcile_*`, `workqueue_*`, `rest_client_requests_total`, `controller_runtime_webhook_requests_total`.
- Others you may already run: cert-manager's `certmanager_certificate_expiration_timestamp_seconds` (the Secret, not the process), kube-state-metrics, and node-exporter (disk and ZFS collectors on storage nodes).

## Trace reference

### How traces are shaped

- **Controller traces** start at a CSI controller call from a sidecar (SP1). Controller→agent calls are CLIENT children (SP2), and the agent's SERVER span (SP5) continues the same trace through W3C `traceparent`.
- **Node traces** start at a CSI node call from kubelet (SP9). They are separate traces: kubelet sends no trace context, and NodeStage can run minutes or days after ControllerPublish. Link a node trace to its controller trace by attribute: both carry `pillar_csi.pv.name`.
- Two rare controller jobs start their own traces: an agent's export restore after a restart (SP3) and the reap of an abandoned provisioning (SP4).
- **Background loops make no traces.** The 30 second health poll and the 30 second per-volume resync start parentless CLIENT spans, and the sampler always drops those. The agent follows that decision.
- Not traced at all: `Probe`, `GetPluginInfo`, the `*GetCapabilities` calls, `NodeGetInfo`, `NodeGetVolumeStats`, `ValidateVolumeCapabilities`, `GetCapacity`, the gRPC health service, the agent's `HealthCheck`, `GetCapabilities` and `GetCapacity`, Kubernetes reconcilers and API client calls, single configfs or sysfs reads and writes, and data-path I/O.

Sampling: `tracing.samplerRatio` is the fraction of new traces kept (SP1, SP3, SP4, SP9, and agent calls from non-pillar clients). Every child follows its parent's decision, so a kept trace is complete from controller to agent. The ratio is parent-based and overrides `OTEL_TRACES_SAMPLER`. Only W3C trace context is propagated; baggage is not.

Names:

- RPC spans come from otelgrpc and are named `<package.Service>/<Method>`, with the standard `rpc.system.name`, `rpc.method`, `rpc.response.status_code`, and on client and agent spans `server.address`, `server.port`.
- pillar-csi's own spans are named `pillar_csi.<component>.<op>`. External command spans are named `exec <executable>[ <subcommand>]`.
- Custom attributes are named `pillar_csi.<entity>.<property>`. Kubernetes and service identity (`k8s.*`, `service.*`) live on the resource, never on spans.

### Spans

| id | name | kind | starts | key attributes | answers |
|---|---|---|---|---|---|
| SP1 | `csi.v1.Controller/CreateVolume`, `/DeleteVolume`, `/ControllerPublishVolume`, `/ControllerUnpublishVolume`, `/ControllerExpandVolume` | SERVER | Root of a controller trace | Common volume attributes; `pillar_csi.store.name`; `pillar_csi.pvc.name`, `pillar_csi.pvc.namespace`; Create/Expand: `pillar_csi.capacity.requested_bytes`, `pillar_csi.capacity.allocated_bytes`; Create: `pillar_csi.create.resumed_from`; Publish/Unpublish: `pillar_csi.target_node.name`; Publish: `pillar_csi.attach_mode`, `pillar_csi.readonly`. Events: `pillar_csi.pvs.update`, `pillar_csi.pvs.aborted`. | What happened to this volume on the controller, was it a retry, what was attached where? |
| SP2 | `pillar_csi.agent.v1.AgentService/<Method>` | CLIENT | Child of SP1, SP3 or SP4 | `pillar_csi.agent.name`, `server.address`, `server.port`, `rpc.*` | Which agent was called, and did the call fail before the agent answered? |
| SP3 | `pillar_csi.controller.restore_exports` | INTERNAL | Root, once per agent restart | `pillar_csi.agent.name`, `pillar_csi.restore.volumes`, `pillar_csi.restore.failed` | Did the agent's export restore succeed? |
| SP4 | `pillar_csi.controller.volume_reap` | INTERNAL | Root, only when a provisioning was found abandoned | Common volume attributes; `pillar_csi.reap.result`: `reaped`, `kept`, `error` (`kept` is not an error) | Did pillar-csi delete this volume's storage on its own? |
| SP5 | `pillar_csi.agent.v1.AgentService/<Method>` | SERVER | Child of SP2; a call from a non-pillar client is a root | `pillar_csi.pv.name`, `pillar_csi.pool.name`, `pillar_csi.backend.type`, `pillar_csi.fence.op`, `pillar_csi.fence.decision`, `pillar_csi.lock.target.wait_duration`; ReconcileState: `pillar_csi.reconcile.complete`, `pillar_csi.reconcile.volumes`, `pillar_csi.reconcile.failed`. Event: `pillar_csi.reconcile.item_failed` (up to 128 per span). | What did the agent do, how long did it wait on the lock, did fencing reject it? |
| SP6 | `pillar_csi.agent.device_wait` | INTERNAL | Child of the ExportVolume SP5 | `pillar_csi.device.path`, `pillar_csi.wait.timeout` | How long did the agent wait for the zvol or LV device? |
| SP7 | `pillar_csi.agent.nvmet.apply`, `.remove`, `.allow_host`, `.deny_host`, `.disable_namespace`, `.enable_namespace`, `.resize_namespace` | INTERNAL | Child of SP5 (not inside ReconcileState) | `pillar_csi.nvme.subsystem_nqn`, `pillar_csi.nvmet.port`, `pillar_csi.nvmet.acl_enabled`; allow/deny: `pillar_csi.nvme.host_nqn`; on error: `pillar_csi.configfs.op`, `pillar_csi.configfs.path` | Which configfs operation failed, on which path? |
| SP8 | `exec <executable>[ <subcommand>]`, e.g. `exec zfs create`, `exec lvcreate`, `exec dmsetup create`, `exec mkfs.xfs`, `exec resize2fs` | INTERNAL | Child of the calling span; never a root | `process.executable.name`, `pillar_csi.exec.subcommand`, `process.exit.code`; on error: `process.command_args` (zfs, lvs, vgs, lvcreate, lvremove, lvextend, dmsetup only), `pillar_csi.exec.output_tail` | Which command failed, with what exit code and output, and how long did it run? |
| SP9 | `csi.v1.Node/NodeStageVolume`, `/NodeUnstageVolume`, `/NodePublishVolume`, `/NodeUnpublishVolume`, `/NodeExpandVolume` | SERVER | Root of a node trace | Common volume attributes; Stage: `pillar_csi.attach_mode`, `pillar_csi.access_type`, `pillar_csi.fs.type`, `pillar_csi.stage.already_staged`; Publish: `pillar_csi.pod.name`, `pillar_csi.pod.namespace`, `pillar_csi.readonly`; Unstage: `pillar_csi.attach_mode`; Expand: `pillar_csi.fs.type`, `pillar_csi.attach_mode` | What happened to this volume on the worker? |
| SP10 | `pillar_csi.node.nvmeof_connect` | INTERNAL | Child of NodeStage | `server.address`, `server.port`, `pillar_csi.nvme.subsystem_nqn`, `pillar_csi.nvme.already_connected`, `pillar_csi.nvme.dying_wait_duration`, `pillar_csi.nvme.hdr_digest`, `pillar_csi.nvme.data_digest` | Did the NVMe-oF connect fail, and was the target rejecting this host? |
| SP11 | `pillar_csi.node.nvmeof_device_wait` | INTERNAL | Child of NodeStage | `pillar_csi.nvme.subsystem_nqn`, `pillar_csi.poll.attempts` | How long did the worker wait for the NVMe device after connect? |
| SP12 | `pillar_csi.node.nvmeof_disconnect` | INTERNAL | Child of NodeUnstage | `pillar_csi.nvme.subsystem_nqn` | Did the disconnect fail? |
| SP13 | `pillar_csi.node.format_and_mount` | INTERNAL | Child of NodeStage (filesystem volumes not already mounted) | `pillar_csi.fs.type`, `pillar_csi.fs.detected`, `pillar_csi.mkfs.performed` | Did pillar-csi format this volume, or was a filesystem already there? The `mkfs` itself is an SP8 child. |

### Attributes

**Common volume attributes.** Set on SP1, SP4 and SP9 from the CSI volume ID `<agent>/<protocol>/<backend>/<pool>/<pv>`:

| attribute | value |
|---|---|
| `pillar_csi.volume.id` | The CSI volume ID. |
| `pillar_csi.pv.name` | The PV name, which is also the PillarVolumeState name. For a Pending PVC it is `pvc-<PVC uid>`. |
| `pillar_csi.agent.name` | PillarAgent name. |
| `pillar_csi.protocol.type` | `nvmeof-tcp` |
| `pillar_csi.backend.type` | `zfs-zvol`, `lvm-lv` |
| `pillar_csi.pool.name` | Pool or VG name. |

The agent only knows `<pool>/<pv>`, so SP5 sets `pillar_csi.pv.name` and `pillar_csi.pool.name` (plus `pillar_csi.backend.type`).

**Controller attributes (SP1).**

| attribute | value |
|---|---|
| `pillar_csi.store.name` | The PillarStore the volume uses. |
| `pillar_csi.pvc.name`, `pillar_csi.pvc.namespace` | The PVC. |
| `pillar_csi.capacity.requested_bytes`, `pillar_csi.capacity.allocated_bytes` | Create and Expand. |
| `pillar_csi.create.resumed_from` | `new`, `provisioning`, `create_partial`, `ready`: where a CreateVolume retry picked up. |
| `pillar_csi.target_node.name` | Publish/Unpublish: the node the volume is published to. Matches the node trace's resource `k8s.node.name`. |
| `pillar_csi.attach_mode` | `local` or `network`. |
| `pillar_csi.readonly` | Whether the volume was published read-only. |

SP1 events: `pillar_csi.pvs.update` after each PillarVolumeState status write, with `pillar_csi.pvs.phase.from`, `pillar_csi.pvs.phase.to`, `pillar_csi.pvs.publication_generation` and `pillar_csi.pvs.conflict_retries`; `pillar_csi.pvs.aborted` when the lifecycle was aborted because the object's UID changed.

**Controller job attributes.** SP3: `pillar_csi.restore.volumes` (volumes to restore), `pillar_csi.restore.failed` (items that failed). SP4: `pillar_csi.reap.result`.

**Agent attributes (SP5, SP6, SP7).**

| attribute | value |
|---|---|
| `pillar_csi.fence.op` | `grant`, `revoke`, `destroy` |
| `pillar_csi.fence.decision` | A [fencing decision](#fencing-decisions). |
| `pillar_csi.lock.target.wait_duration` | Seconds spent waiting on per-target locks. |
| `pillar_csi.reconcile.complete`, `pillar_csi.reconcile.volumes`, `pillar_csi.reconcile.failed` | ReconcileState outcome. |
| `pillar_csi.device.path`, `pillar_csi.wait.timeout` | SP6: the device waited for and the timeout in seconds. |
| `pillar_csi.nvme.subsystem_nqn`, `pillar_csi.nvmet.port`, `pillar_csi.nvmet.acl_enabled`, `pillar_csi.nvme.host_nqn` | SP7 target and initiator. |
| `pillar_csi.configfs.op`, `pillar_csi.configfs.path` | SP7 on error: the failed operation and path relative to the configfs root. |

ReconcileState event `pillar_csi.reconcile.item_failed`: one per failed volume, with `pillar_csi.pv.name`, `pillar_csi.reconcile.phase` (`resolve`, `prepare`, `link`; for iSCSI, `link` is the portal and TPG enable) and `error.type`.

**Exec attributes (SP8).** `process.executable.name` (basename), `pillar_csi.exec.subcommand` (same values as the M9 label, omitted when empty), `process.exit.code`. On error only: `process.command_args` and `pillar_csi.exec.output_tail` (last 1 KiB of output).

**Node attributes (SP9 to SP13).**

| attribute | value |
|---|---|
| `pillar_csi.attach_mode` | `local` or `network`. |
| `pillar_csi.access_type` | `mount` or `block`. |
| `pillar_csi.fs.type` | Requested filesystem. |
| `pillar_csi.stage.already_staged` | NodeStage found the volume already staged. |
| `pillar_csi.pod.name`, `pillar_csi.pod.namespace` | NodePublish: the Pod. |
| `pillar_csi.readonly` | NodePublish read-only. |
| `server.address`, `server.port` | SP10: target address and port. |
| `pillar_csi.nvme.already_connected` | SP10: the controller was already connected. |
| `pillar_csi.nvme.dying_wait_duration` | SP10: seconds spent waiting for old controllers to go away. |
| `pillar_csi.nvme.hdr_digest`, `pillar_csi.nvme.data_digest` | SP10: the publish context requested the NVMe/TCP header / data digest. With `already_connected` the existing connection was reused and keeps its own digest settings. |
| `pillar_csi.poll.attempts` | SP11: polls until the device appeared. |
| `pillar_csi.fs.detected` | SP13: filesystem found on the device (`""` = blank). |
| `pillar_csi.mkfs.performed` | SP13: `true` if `mkfs` ran. |

### Errors on spans

- **SERVER spans (SP1, SP5, SP9):** status follows otelgrpc. In addition, every non-OK call sets `error.type` to the canonical code, e.g. `FAILED_PRECONDITION`, so client errors are searchable too: `{ span.error.type != nil }`.
- **CLIENT spans (SP2):** status Error and `rpc.response.status_code` on any non-OK code.
- **INTERNAL spans:** status Error with the message (up to 512 bytes), and `error.type` set to the first that applies:
  - the span's own value: SP6 and SP11 `timeout`; SP7 `port_inline_data_size_conflict`, `port_mdts_conflict` or `device_held`; SP8 `exit_<code>`, `start_error`, `context_canceled`, `deadline_exceeded`; SP10 the errno of the `/dev/nvme-fabrics` write (`EIO` usually means the target rejected this host's NQN);
  - the canonical gRPC code for a gRPC status error;
  - the errno name, such as `EBUSY`;
  - `timeout` for a context deadline;
  - `_OTHER`.

An NVMe device wait timeout (SP11 `error.type=timeout`) reaches Kubernetes as NodeStage code `Internal`, not `DeadlineExceeded`.

## Find the traces of a PVC, PV or Pod

Every volume span carries `pillar_csi.pv.name`. Get the PV name of a PVC:

```sh
kubectl get pvc data -n app -o jsonpath='{.spec.volumeName}'
# Pending PVC with no PV yet: the PV name will be pvc-<PVC uid>
echo "pvc-$(kubectl get pvc data -n app -o jsonpath='{.metadata.uid}')"
```

TraceQL (Grafana Tempo):

```traceql
# Every trace of the volume: controller, agent and node
{ span.pillar_csi.pv.name = "pvc-0b8c7a52-..." }

# Only the failures
{ span.pillar_csi.pv.name = "pvc-0b8c7a52-..." && status = error }

# By PVC name (controller spans only)
{ span.pillar_csi.pvc.name = "data" && span.pillar_csi.pvc.namespace = "app" }

# By Pod (NodePublishVolume)
{ span.pillar_csi.pod.name = "web-0" && span.pillar_csi.pod.namespace = "app" }

# The node side of a publish: node traces for the volume on the published node
{ resource.service.name = "pillar-csi-node" && resource.k8s.node.name = "worker-1"
  && span.pillar_csi.pv.name = "pvc-0b8c7a52-..." }
```

For the last query, take the node name from `pillar_csi.target_node.name` on the controller's ControllerPublishVolume span.

Jaeger: pick the service (`pillar-csi-controller`, `pillar-csi-agent` or `pillar-csi-node`) and enter the tag `pillar_csi.pv.name=pvc-0b8c7a52-...` in **Tags**. Add `error=true` to see only failed spans. Search each service separately, since node traces are not children of controller traces.

If nothing shows up: check `tracing.enabled`, that the backend receives OTLP/gRPC, and `tracing.samplerRatio`. With a ratio below 1, some operations are not traced.

## Go from a failure log line to its trace

Each failed gRPC call on the controller, agent and node writes exactly one log line with `msg="rpc failed"`. Successful calls log nothing. Codes `Unknown`, `DeadlineExceeded`, `Unimplemented`, `Internal`, `Unavailable` and `DataLoss` log at `ERROR`; other codes at `WARN`.

| field | value |
|---|---|
| `grpc.method` | Full method, e.g. `/csi.v1.Controller/CreateVolume`. |
| `grpc.code` | Code name, e.g. `Unavailable`. |
| `duration_seconds` | How long the call ran. |
| `error` | The status message (the same text as the PVC or Pod Event). |
| `pillar_csi.volume.id` | CSI calls that have a volume ID. For CreateVolume, `pillar_csi.pv.name` holds the requested name instead. |
| `pillar_csi.pv.name` | Agent calls. |
| `pillar_csi.fence.decision` | Agent calls that went through fencing. |
| `trace_id`, `span_id` | Only when the call was sampled. |

The agent and node write these lines as JSON on stderr. The controller writes them through its usual logger as the `csi` logger.

```sh
kubectl logs -n pillar-csi -l app.kubernetes.io/component=agent -c agent --tail=-1 \
  | grep '"msg":"rpc failed"'
```

```json
{"time":"2026-09-30T10:12:03Z","level":"ERROR","msg":"rpc failed","grpc.method":"/pillar_csi.agent.v1.AgentService/ExportVolume","grpc.code":"Internal","duration_seconds":30.2,"error":"...","pillar_csi.pv.name":"pvc-0b8c7a52-...","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"00f067aa0ba902b7"}
```

Open the trace:

- Tempo: paste the `trace_id` into the Grafana Explore TraceQL box, or configure a Loki derived field with the regex `"trace_id":"(\w+)"` that links to Tempo.
- Jaeger: open `/trace/<trace_id>`, or paste it into the search box.

No `trace_id` means the call was not sampled or tracing is off. Use `pillar_csi.pv.name` or `pillar_csi.volume.id` from the line to [search by volume](#find-the-traces-of-a-pvc-pv-or-pod) instead.

The selector matches the chart's component label. Add `app.kubernetes.io/instance=<release>` if you run more than one release.

Going the other way, a latency spike on `grpc_server_handling_seconds` or `pillar_csi_exec_duration_seconds` has `trace_id` exemplars when exemplar storage is on. Grafana shows them as dots on the graph that open the trace.

## Did an agent call fail in transport or in the agent?

The controller counts every call it makes (`pillar_csi_agent_client_requests_total`); the agent counts every call it handles (`grpc_server_handled_total`). A failure the controller sees but the agent does not happened in transport: DNS, dial, connection refused, TLS handshake, or a deadline before the agent answered.

```promql
# Failures seen by the controller, per agent and method
sum by (agent, method) (
  rate(pillar_csi_agent_client_requests_total{code!="OK"}[5m])
)

# Failures returned by the agent handlers, per agent pod and method
sum by (pod, grpc_method) (
  rate(grpc_server_handled_total{grpc_service="pillar_csi.agent.v1.AgentService", grpc_code!="OK"}[5m])
)
```

- Controller failures with no matching agent failures: transport. Typical codes are `Unavailable` and `DeadlineExceeded`. Check the network, the agent's port 9500, and TLS settings.
- Matching failures on both sides: the agent handler failed. Its `grpc_code` and the SP5 span tell you why.
- The `agent` label is the PillarAgent name; the agent series are labeled by pod and node. Map them with `kubectl get pillaragent`.

In a trace, a transport failure is an SP2 CLIENT span with status Error and no SP5 SERVER child.

## Example alert rules

These are examples, **not shipped in the chart**. Tune thresholds and `for` durations to your environment. They assume prometheus-operator; drop the wrapper for plain Prometheus.

The pool metrics have no `agent` label. If two agents use the same pool name, the `on(pool)` joins below match both; add an agent label to the agent PodMonitor through relabeling if you need to tell them apart.

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: pillar-csi
  namespace: pillar-csi
spec:
  groups:
    - name: pillar-csi.capacity
      rules:
        - alert: PillarStoreSpaceLow
          expr: |
            (pillar_csi_pool_available_bytes / pillar_csi_pool_size_bytes < 0.10)
              * on(pool) group_left(store) pillar_csi_store_info
          for: 15m
          labels: {severity: warning}
          annotations:
            summary: "Store {{ $labels.store }} (pool {{ $labels.pool }}) has less than 10% space left"
        - alert: PillarStoreFullIn24h
          expr: |
            (predict_linear(pillar_csi_pool_available_bytes[6h], 24 * 3600) < 0)
              * on(pool) group_left(store) pillar_csi_store_info
          for: 1h
          labels: {severity: warning}
          annotations:
            summary: "Store {{ $labels.store }} will run out of space within 24 hours at the current rate"
        - alert: PillarThinMetadataHigh
          expr: pillar_csi_pool_thin_metadata_used_ratio > 0.80
          for: 15m
          labels: {severity: warning}
          annotations:
            summary: "LVM thin pool {{ $labels.pool }} metadata is {{ $value | humanizePercentage }} full"

    - name: pillar-csi.agents
      rules:
        - alert: PillarAgentDisconnected
          expr: |
            pillar_csi_resource_status_condition{kind="PillarAgent", type="AgentConnected", status!="True"} == 1
          for: 5m
          labels: {severity: critical}
          annotations:
            summary: "PillarAgent {{ $labels.name }} is not connected ({{ $labels.reason }})"
        - alert: PillarAgentSubsystemDegraded
          expr: pillar_csi_agent_subsystem_healthy == 0
          for: 5m
          labels: {severity: critical}
          annotations:
            summary: "Agent on {{ $labels.pod }}: {{ $labels.subsystem }} {{ $labels.pool }} is unhealthy"
        - alert: PillarExportRestoreStuck
          expr: |
            pillar_csi_resource_status_condition{kind="PillarAgent", type="ExportsReady",
              reason=~"ExportRestorePending|ExportRestoreFailed"} == 1
          for: 15m
          labels: {severity: critical}
          annotations:
            summary: "PillarAgent {{ $labels.name }} has not restored its exports ({{ $labels.reason }})"
        - alert: PillarAgentRPCErrors
          expr: |
            sum by (agent, method, code) (
              rate(pillar_csi_agent_client_requests_total{
                code=~"Unknown|DeadlineExceeded|Unimplemented|Internal|Unavailable|DataLoss"}[5m])
            ) > 0
          for: 10m
          labels: {severity: warning}
          annotations:
            summary: "Calls to agent {{ $labels.agent }} {{ $labels.method }} fail with {{ $labels.code }}"
        - alert: PillarAgentOperationHung
          expr: |
            sum by (namespace, pod, grpc_method) (
              grpc_server_started_total{grpc_service="pillar_csi.agent.v1.AgentService", grpc_type="unary"})
            - sum by (namespace, pod, grpc_method) (
              grpc_server_handled_total{grpc_service="pillar_csi.agent.v1.AgentService", grpc_type="unary"})
            > 0
          for: 10m
          labels: {severity: warning}
          annotations:
            summary: "Agent {{ $labels.pod }} has had {{ $labels.grpc_method }} in flight for 10 minutes"

    - name: pillar-csi.volumes
      rules:
        - alert: PillarVolumesStuckCreatePartial
          expr: sum by (agent) (pillar_csi_volumes{phase="CreatePartial"}) > 0
          for: 30m
          labels: {severity: warning}
          annotations:
            summary: "{{ $value }} volumes on agent {{ $labels.agent }} are stuck half-created"
        - alert: PillarExportsUnreconciled
          expr: sum by (agent, reason) (pillar_csi_volumes_export_unreconciled) > 0
          for: 15m
          labels: {severity: warning}
          annotations:
            summary: "{{ $value }} exports on agent {{ $labels.agent }} are unreconciled ({{ $labels.reason }})"
        - alert: PillarVolumesReaped
          expr: increase(pillar_csi_volumes_reaped_total{result="reaped"}[1h]) > 0
          labels: {severity: info}
          annotations:
            summary: "pillar-csi deleted abandoned volume storage on agent {{ $labels.agent }}"
        - alert: PillarVolumeReapFailed
          expr: increase(pillar_csi_volumes_reaped_total{result="error"}[1h]) > 0
          labels: {severity: warning}
          annotations:
            summary: "Reaping an abandoned volume on agent {{ $labels.agent }} failed"
        - alert: PillarFencingRejections
          expr: |
            sum by (fence_op, decision) (
              rate(pillar_csi_agent_fencing_decisions_total{decision=~"reject_.*|mark_io_error"}[5m])
            ) > 0
          for: 10m
          labels: {severity: warning}
          annotations:
            summary: "Agent fencing returns {{ $labels.decision }} for {{ $labels.fence_op }}"

    - name: pillar-csi.nodes
      rules:
        - alert: PillarNVMeoFPathDegraded
          expr: pillar_csi_node_nvmeof_controllers{state!="live"} > 0
          for: 5m
          labels: {severity: warning}
          annotations:
            summary: "Node {{ $labels.pod }} path to {{ $labels.target_address }} is {{ $labels.state }}"

    - name: pillar-csi.certificates
      rules:
        - alert: PillarLoadedCertificateExpiring
          expr: pillar_csi_tls_certificate_not_after_timestamp_seconds - time() < 7 * 24 * 3600
          labels: {severity: warning}
          annotations:
            summary: "{{ $labels.pod }} uses a {{ $labels.role }} certificate that expires in under 7 days; restart it to load the renewed one"
```

## Known limitation: mTLS breaks the CSI to agent path

With `mtls.enabled=true`, the controller's CSI calls to the agent (CreateVolume, publish, expand, delete, export restore and resync) still dial the agent without TLS, while the agent requires a client certificate. Only the PillarAgent health check uses mTLS. So the agent looks connected and authenticated, but every volume operation fails. Keep `mtls.enabled=false` until this is fixed.

How it shows up in telemetry:

- `pillar_csi_agent_client_requests_total{method!="HealthCheck", code="Unavailable"}` rises for the agent, while `{method="HealthCheck", code="OK"}` keeps rising.
- `pillar_csi_resource_status_condition{kind="PillarAgent", type="AgentConnected", status="True"}` stays 1 (reason `Authenticated`).
- The agent's `grpc_server_handled_total` has no matching failures: the handshake fails before any handler runs. This is the transport signature from [the query above](#did-an-agent-call-fail-in-transport-or-in-the-agent).
- In traces, the SP1 span fails, and its SP2 CLIENT children end in `Unavailable` with no SP5 SERVER child.
