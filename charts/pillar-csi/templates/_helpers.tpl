{{/*
Expand the name of the chart.
*/}}
{{- define "pillar-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "pillar-csi.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "pillar-csi.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Resolve the namespace for resources (supports namespaceOverride).
*/}}
{{- define "pillar-csi.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride }}
{{- end }}

{{/*
Common labels applied to all resources.
*/}}
{{- define "pillar-csi.labels" -}}
helm.sh/chart: {{ include "pillar-csi.chart" . }}
{{ include "pillar-csi.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels used in matchLabels and Pod labels.
*/}}
{{- define "pillar-csi.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pillar-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Component-specific selector labels.
Usage: {{ include "pillar-csi.componentSelectorLabels" (dict "root" . "component" "agent") }}
*/}}
{{- define "pillar-csi.componentSelectorLabels" -}}
{{ include "pillar-csi.selectorLabels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/*
Component-specific common labels.
Usage: {{ include "pillar-csi.componentLabels" (dict "root" . "component" "agent") }}
*/}}
{{- define "pillar-csi.componentLabels" -}}
{{ include "pillar-csi.labels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/*
Resolve image reference: <repository>:<tag>.
Falls back to .Chart.AppVersion when tag is empty.
Usage: {{ include "pillar-csi.image" (dict "image" .Values.agent.image "defaultTag" .Chart.AppVersion) }}
*/}}
{{- define "pillar-csi.image" -}}
{{- $tag := default .defaultTag .image.tag }}
{{- printf "%s:%s" .image.repository $tag }}
{{- end }}

{{/*
Resolve imagePullPolicy: uses component-level override if set, else global.
Usage: {{ include "pillar-csi.imagePullPolicy" (dict "image" .Values.agent.image "global" .Values.imagePullPolicy) }}
*/}}
{{- define "pillar-csi.imagePullPolicy" -}}
{{- default .global .image.pullPolicy }}
{{- end }}

{{/*
Controller-specific selector labels (stable — never change after deploy).
*/}}
{{- define "pillar-csi.controllerSelectorLabels" -}}
app.kubernetes.io/name: {{ include "pillar-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: controller
{{- end }}

{{/*
ServiceAccount name for the controller.
*/}}
{{- define "pillar-csi.controllerServiceAccountName" -}}
{{- if .Values.serviceAccount.controller.name }}
{{- .Values.serviceAccount.controller.name }}
{{- else }}
{{- printf "%s-controller" (include "pillar-csi.fullname" .) }}
{{- end }}
{{- end }}

{{/*
ServiceAccount name for the node DaemonSet.
*/}}
{{- define "pillar-csi.nodeServiceAccountName" -}}
{{- if .Values.serviceAccount.node.name }}
{{- .Values.serviceAccount.node.name }}
{{- else }}
{{- printf "%s-node" (include "pillar-csi.fullname" .) }}
{{- end }}
{{- end }}

{{/*
ServiceAccount name for the agent DaemonSet.
*/}}
{{- define "pillar-csi.agentServiceAccountName" -}}
{{- if .Values.serviceAccount.agent.name }}
{{- .Values.serviceAccount.agent.name }}
{{- else }}
{{- printf "%s-agent" (include "pillar-csi.fullname" .) }}
{{- end }}
{{- end }}

{{/*
mTLS — directory inside controller and agent containers where the cert
Secret is mounted.  Defaults to /etc/pillar-csi/mtls when mtls.certDir
is unset.
*/}}
{{- define "pillar-csi.mtls.certDir" -}}
{{- default "/etc/pillar-csi/mtls" .Values.mtls.certDir }}
{{- end }}

{{/*
mTLS — name of the Secret holding the CONTROLLER mTLS cert chain.
When cert-manager mode is enabled the chart auto-creates a Secret named
"<fullname>-controller-mtls"; otherwise the operator-provided Secret
name from mtls.secretRefs.controller.secretName is used.
*/}}
{{- define "pillar-csi.mtls.controllerSecret" -}}
{{- if .Values.mtls.certManager.enabled -}}
{{ printf "%s-controller-mtls" (include "pillar-csi.fullname" .) }}
{{- else -}}
{{ .Values.mtls.secretRefs.controller.secretName }}
{{- end -}}
{{- end }}

{{/*
mTLS — name of the Secret holding the AGENT mTLS cert chain.
Same dispatch logic as the controller variant.
*/}}
{{- define "pillar-csi.mtls.agentSecret" -}}
{{- if .Values.mtls.certManager.enabled -}}
{{ printf "%s-agent-mtls" (include "pillar-csi.fullname" .) }}
{{- else -}}
{{ .Values.mtls.secretRefs.agent.secretName }}
{{- end -}}
{{- end }}

{{/*
mTLS — TLS server name the controller uses for SNI / SAN verification
when dialing the agent.  Resolution order:
  1. explicit .Values.mtls.serverName override (operator-managed Secrets)
  2. cert-manager auto-issuance: "<fullname>-agent.<namespace>.svc"
     (matches the dnsNames the chart's cert-manager template renders)
  3. empty string (controller derives from the resolved agent address)
*/}}
{{- define "pillar-csi.mtls.serverName" -}}
{{- if .Values.mtls.serverName -}}
{{ .Values.mtls.serverName }}
{{- else if .Values.mtls.certManager.enabled -}}
{{ printf "%s-agent.%s.svc" (include "pillar-csi.fullname" .) (include "pillar-csi.namespace" .) }}
{{- end -}}
{{- end }}

{{/*
Agent placement config file (mounted into the agent at
pillar-csi.agent.configPath and passed via --config).  .Values.agent.backends
is rendered verbatim: its entries already have the shape the agent decodes
(same keys as PillarStore.spec.backend), and the agent rejects unknown fields
with their path.  The chart checks only what would otherwise ship a
crash-looping DaemonSet:
  - every entry sets exactly one of zfs or lvm;
  - the routing key (zfs.pool / lvm.volumeGroup) is set;
  - no routing key appears twice.  Volumes are routed to a backend by
    pool/VG name alone, so a ZFS pool and an LVM VG must not share a name
    either.  Keys are trimmed so " tank " and "tank" collide.
*/}}
{{- define "pillar-csi.agent.config" -}}
{{- $backends := .Values.agent.backends | default list }}
{{- if not (kindIs "slice" $backends) }}
{{- fail "agent.backends must be a list of {zfs: {...}} or {lvm: {...}} entries" }}
{{- end }}
{{- $seen := dict }}
{{- range $i, $entry := $backends }}
{{- if not (kindIs "map" $entry) }}
{{- fail (printf "agent.backends[%d] must be a mapping with exactly one of zfs or lvm" $i) }}
{{- end }}
{{- $members := keys $entry | sortAlpha }}
{{- if ne (len $members) 1 }}
{{- fail (printf "agent.backends[%d]: exactly one of zfs or lvm must be set, got %v" $i $members) }}
{{- end }}
{{- $member := first $members }}
{{- $body := get $entry $member }}
{{- if not (kindIs "map" $body) }}
{{- fail (printf "agent.backends[%d].%s must be a mapping" $i $member) }}
{{- end }}
{{- $key := "" }}
{{- if eq $member "zfs" }}
{{- $key = required (printf "agent.backends[%d].zfs.pool is required" $i) (get $body "pool") }}
{{- else if eq $member "lvm" }}
{{- $key = required (printf "agent.backends[%d].lvm.volumeGroup is required" $i) (get $body "volumeGroup") }}
{{- else }}
{{- fail (printf "agent.backends[%d].%s is not a supported backend; use zfs or lvm" $i $member) }}
{{- end }}
{{- $normKey := trim (toString $key) }}
{{- if hasKey $seen $normKey }}
{{- fail (printf "agent.backends: pool/VG %q appears in more than one entry; each ZFS pool and LVM VG name must be unique across agent.backends" $normKey) }}
{{- end }}
{{- $_ := set $seen $normKey true }}
{{- end }}
{{- if $backends -}}
backends:
{{- toYaml $backends | nindent 2 }}
{{- else -}}
backends: []
{{- end }}
{{- end }}

{{/*
Directory and file path of the agent config file inside the agent container.
*/}}
{{- define "pillar-csi.agent.configDir" -}}
/etc/pillar-agent
{{- end }}

{{- define "pillar-csi.agent.configPath" -}}
{{ include "pillar-csi.agent.configDir" . }}/config.yaml
{{- end }}

{{/*
OTEL_* environment for the controller, agent and node containers, rendered
only when tracing.enabled.  OTEL_RESOURCE_ATTRIBUTES references
$(POD_NAMESPACE), $(POD_NAME) and $(NODE_NAME), and kubelet expands $(VAR)
only from entries listed EARLIER in the same env list, so every caller must
render those three variables (and HOST_IP, which tracing.endpoint may
reference) before including this helper.  extraEnv stays after it as the
override path.
Usage: include "pillar-csi.otelEnv" . | trim | nindent 12
*/}}
{{- define "pillar-csi.otelEnv" -}}
{{- if .Values.tracing.enabled }}
{{- $endpoint := trim (toString .Values.tracing.endpoint) }}
{{- if not $endpoint }}
{{- fail "tracing.endpoint is required when tracing.enabled is true (e.g. http://tempo-distributor.monitoring:4317)" }}
{{- end }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ $endpoint | quote }}
- name: OTEL_EXPORTER_OTLP_INSECURE
  value: {{ .Values.tracing.insecure | toString | quote }}
- name: OTEL_TRACES_SAMPLER_ARG
  value: {{ .Values.tracing.samplerRatio | toString | quote }}
- name: OTEL_RESOURCE_ATTRIBUTES
  value: "k8s.namespace.name=$(POD_NAMESPACE),k8s.pod.name=$(POD_NAME),k8s.node.name=$(NODE_NAME)"
{{- end }}
{{- end }}

{{/*
Downward-API env shared by the agent and node containers when metrics or
tracing is enabled: HOST_IP feeds --metrics-bind-address=[$(HOST_IP)]:<port>
(and a node-local tracing.endpoint), POD_NAME feeds OTEL_RESOURCE_ATTRIBUTES.
Must render before pillar-csi.otelEnv.
*/}}
{{- define "pillar-csi.telemetryPodEnv" -}}
{{- if or .Values.metrics.enabled .Values.tracing.enabled }}
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: HOST_IP
  valueFrom:
    fieldRef:
      fieldPath: status.hostIP
{{- end }}
{{- end }}

{{/*
--metrics-bind-address value for a hostNetwork-capable DaemonSet.  Under
hostNetwork the plaintext endpoint binds only the node's primary IP instead
of every host interface; brackets keep IPv6 host IPs valid.
Usage: {{ include "pillar-csi.metricsBindAddress" (dict "hostNetwork" .Values.agent.hostNetwork "port" .Values.metrics.agent.port) }}
*/}}
{{- define "pillar-csi.metricsBindAddress" -}}
{{- if .hostNetwork -}}
[$(HOST_IP)]:{{ .port }}
{{- else -}}
:{{ .port }}
{{- end -}}
{{- end }}

{{/*
Name of the metrics-reader ClusterRole, ServiceAccount and binding; the
token Secret the controller PodMonitor authenticates with is "<name>-token".
*/}}
{{- define "pillar-csi.metricsReaderName" -}}
{{- printf "%s-metrics-reader" (include "pillar-csi.fullname" .) }}
{{- end }}

{{/*
Optional interval/scrapeTimeout fields shared by every PodMonitor endpoint.
*/}}
{{- define "pillar-csi.podMonitorScrapeFields" -}}
{{- with .Values.metrics.podMonitor.interval }}
interval: {{ . | quote }}
{{- end }}
{{- with .Values.metrics.podMonitor.scrapeTimeout }}
scrapeTimeout: {{ . | quote }}
{{- end }}
{{- end }}
