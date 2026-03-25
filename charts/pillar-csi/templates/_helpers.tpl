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
