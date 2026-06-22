# Install with Helm

Helm is the recommended way to install the Percona Operator for Valkey. Two charts ship as
a pair:

| Chart | Installs |
|-------|----------|
| **`valkey-operator`** | the operator Deployment, RBAC (namespaced or cluster-wide), the `ServiceAccount`, and the CRDs (under `crds/`). |
| **`valkey-db`** | a `PerconaValkeyCluster` CR plus its referenced Secrets (ACL users, TLS) and backup storages. |

A standalone **`valkey-operator-crds`** chart is also available for managing the CRD
lifecycle separately (useful in GitOps).

## Add the repository

```bash
helm repo add percona https://percona.github.io/percona-helm-charts/
helm repo update
```

## Install the operator

```bash
helm install valkey-operator percona/valkey-operator \
  --namespace valkey-operator --create-namespace
```

### Watch scope

The operator can watch a single namespace (default, least privilege) or all namespaces.

=== "Namespaced (default)"

    ```bash
    helm install valkey-operator percona/valkey-operator \
      --namespace valkey-operator --create-namespace \
      --set watchAllNamespaces=false
    ```

    Installs a namespaced `Role`/`RoleBinding`.

=== "Cluster-wide"

    ```bash
    helm install valkey-operator percona/valkey-operator \
      --namespace valkey-operator --create-namespace \
      --set watchAllNamespaces=true
    ```

    Installs a `ClusterRole`/`ClusterRoleBinding`; the operator watches every namespace.

### Managing CRDs separately

Set `installCRDs=false` and install the CRD-only chart, e.g. for GitOps where CRD lifecycle
is owned by a platform team:

```bash
helm install valkey-crds percona/valkey-operator-crds
helm install valkey-operator percona/valkey-operator --set installCRDs=false
```

## Create a Valkey cluster

```bash
helm install my-valkey percona/valkey-db \
  --namespace valkey --create-namespace \
  --values my-valkey-values.yaml
```

The `valkey-db` chart's `values.yaml` keys map 1:1 to the `PerconaValkeyCluster` spec — see
[Configuration](configuration.md) for the full surface. A minimal cluster:

```yaml
mode: cluster
shards: 3
replicas: 1
persistence:
  enabled: true
  size: 20Gi
exporter:
  enabled: true
```

## Image pins

The chart default image tags are a hand-edited copy of the operator's pinned GA images.
For this release:

| Component | Image |
|-----------|-------|
| Operator | `percona/valkey-operator:{{ release }}` |
| Server | `percona/percona-valkey:{{ valkeydefaultrecommended }}` |
| Backup | `percona/valkey-backup:{{ backuprecommended }}` |
| Exporter | `percona/valkey-exporter:{{ exporterrecommended }}` |

!!! warning "Use a compatible set"

    All containers in a Valkey pod must be a compatible set — the exporter must understand
    the running engine and the backup tool must match the server's RDB/AOF format. There is
    no version negotiation. Do not bump one image in isolation; see
    [Versions compatibility](versions.md).

## Upgrade the chart

```bash
helm repo update
helm -n valkey-operator upgrade valkey-operator percona/valkey-operator
```

See [Upgrades](upgrades.md) for the relationship between the chart `appVersion`, the
operator version, and `spec.crVersion`.
