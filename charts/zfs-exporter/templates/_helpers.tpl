{{/* The chart name. A value can override it. */}}
{{- define "zfs-exporter.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
The fully qualified name. It carries the release name as a prefix, unless the
release already has the name of the chart. That case would give
"zfs-exporter-zfs-exporter".
*/}}
{{- define "zfs-exporter.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "zfs-exporter.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "zfs-exporter.labels" -}}
helm.sh/chart: {{ include "zfs-exporter.chart" . }}
{{ include "zfs-exporter.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "zfs-exporter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "zfs-exporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Selector labels for the exporter pods alone. The device-plugin pods carry the
two labels above as well, so those two match both workloads. The DaemonSet
selector cannot carry the component label, because spec.selector is immutable.
It goes on the pod template instead, which keeps the selector a subset.
*/}}
{{- define "zfs-exporter.exporterSelectorLabels" -}}
{{ include "zfs-exporter.selectorLabels" . }}
app.kubernetes.io/component: exporter
{{- end -}}

{{/* A setting that would otherwise fail silently. */}}
{{- define "zfs-exporter.validateValues" -}}
{{- if and .Values.serviceMonitor.enabled (not .Values.service.enabled) -}}
{{- fail "zfs-exporter: serviceMonitor.enabled needs service.enabled. A ServiceMonitor selects a Service, so without one it finds no target and reports no error." -}}
{{- end -}}
{{- end -}}

{{/*
The image reference. A digest overrides a tag. A digest is what makes a
rollout reproducible, and a value that sets both silently ignores the tag.
*/}}
{{- define "zfs-exporter.image" -}}
{{- $repo := printf "%s/%s" .Values.image.registry .Values.image.repository -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" $repo .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (default .Chart.Version .Values.image.tag) -}}
{{- end -}}
{{- end -}}
