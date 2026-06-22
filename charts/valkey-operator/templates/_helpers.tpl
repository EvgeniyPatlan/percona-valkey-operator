{{/*
Helpers for the valkey-operator chart. Names mirror the deploy/ manifests
(deploy/operator.yaml, deploy/rbac.yaml, deploy/cw-*.yaml) so a chart install
renders the same resources the kustomize install does.
*/}}

{{/* Chart name, overridable. */}}
{{- define "valkey-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name. The deploy/ manifests hardcode `valkey-operator`;
keep that as the default release name so RBAC subjects/roleRefs line up, but
allow an override for multi-tenant installs.
*/}}
{{- define "valkey-operator.fullname" -}}
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

{{/* Chart label value (name-version). */}}
{{- define "valkey-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Common labels applied to every rendered resource. */}}
{{- define "valkey-operator.labels" -}}
helm.sh/chart: {{ include "valkey-operator.chart" . }}
{{ include "valkey-operator.selectorLabels" . }}
app.kubernetes.io/component: manager
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: percona-valkey-operator
{{- end -}}

{{/* Selector labels (the immutable subset). */}}
{{- define "valkey-operator.selectorLabels" -}}
app.kubernetes.io/name: percona-valkey-operator
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* ServiceAccount name. */}}
{{- define "valkey-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "valkey-operator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Operator image reference. tag defaults to .Chart.AppVersion (the operator
version) when image.tag is empty — the single operator-axis source of truth.
*/}}
{{- define "valkey-operator.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
WATCH_NAMESPACE value:
  - watchAllNamespaces=true  => "" (cluster-wide watch)
  - watchNamespace set       => that literal value
  - otherwise                => the release namespace (namespaced)
*/}}
{{- define "valkey-operator.watchNamespace" -}}
{{- if .Values.watchAllNamespaces -}}
{{- "" -}}
{{- else if .Values.watchNamespace -}}
{{- .Values.watchNamespace -}}
{{- else -}}
{{- .Release.Namespace -}}
{{- end -}}
{{- end -}}
