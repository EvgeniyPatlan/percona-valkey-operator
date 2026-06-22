{{/*
Operator operational RBAC rules — the single rule set used by BOTH the
namespaced Role (watchAllNamespaces=false) and the cluster-wide ClusterRole
(watchAllNamespaces=true). Kept in one place so the two scopes never drift.
Mirrors the `valkey-operator` (Cluster)Role in deploy/rbac.yaml / deploy/cw-rbac.yaml.
No wildcard "*" resources (arch 07 §6).
*/}}
{{- define "valkey-operator.rules" -}}
- apiGroups: [""]
  resources: ["configmaps", "persistentvolumeclaims", "secrets", "services"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: [""]
  resources: ["persistentvolumeclaims/status"]
  verbs: ["get"]
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["apps"]
  resources: ["deployments", "statefulsets"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
- apiGroups: ["batch"]
  resources: ["jobs"]
  verbs: ["create", "delete", "get", "list", "watch"]
- apiGroups: ["cert-manager.io"]
  resources: ["certificates"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
- apiGroups: ["networking.k8s.io"]
  resources: ["networkpolicies"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
- apiGroups: ["policy"]
  resources: ["poddisruptionbudgets"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
- apiGroups: ["valkey.percona.com"]
  resources: ["perconavalkeybackups", "perconavalkeyclusters", "perconavalkeyrestores", "valkeynodes"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
- apiGroups: ["valkey.percona.com"]
  resources: ["perconavalkeybackups/finalizers", "perconavalkeyclusters/finalizers", "perconavalkeyrestores/finalizers", "valkeynodes/finalizers"]
  verbs: ["update"]
- apiGroups: ["valkey.percona.com"]
  resources: ["perconavalkeybackups/status", "perconavalkeyclusters/status", "perconavalkeyrestores/status", "valkeynodes/status"]
  verbs: ["get", "patch", "update"]
{{- end -}}
