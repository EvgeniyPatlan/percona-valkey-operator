{{/*
Helpers for the valkey-db chart. This chart authors a single
PerconaValkeyCluster CR (plus optional referenced Secrets); the operator owns
the workloads.
*/}}

{{- define "valkey-db.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
The CR name. Defaults to the release name (so `helm install my-valkey ...`
yields a PerconaValkeyCluster named my-valkey), overridable.
*/}}
{{- define "valkey-db.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "valkey-db.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "valkey-db.labels" -}}
helm.sh/chart: {{ include "valkey-db.chart" . }}
app.kubernetes.io/name: {{ include "valkey-db.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: percona-valkey-operator
{{- end -}}

{{/* Default-user Secret name: explicit override, else <cluster>-users. */}}
{{- define "valkey-db.defaultUserSecretName" -}}
{{- if .Values.auth.passwordSecret.name -}}
{{- .Values.auth.passwordSecret.name -}}
{{- else -}}
{{- printf "%s-users" (include "valkey-db.fullname" .) -}}
{{- end -}}
{{- end -}}
