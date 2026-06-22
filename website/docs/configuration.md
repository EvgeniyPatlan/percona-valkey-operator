# Configuration

The `PerconaValkeyCluster` custom resource is the single object you write. With the
`valkey-db` Helm chart, every `values.yaml` key maps 1:1 to a `spec.*` field, so the chart
is a thin render of your values into the CR. This page documents the configuration surface.

!!! note

    Secrets (ACL user passwords, TLS material, backup-storage credentials) are always
    **referenced** by name — never inlined into the CR or chart values.

## Topology

| Field | Values | Meaning |
|-------|--------|---------|
| `spec.mode` | `cluster` \| `replication` | Sharded cluster (16384 slots) or 1 primary + N replicas. |
| `spec.shards` | integer | Number of primary shards (cluster mode). |
| `spec.replicas` | integer | Replicas per shard / primary. |
| `spec.workloadType` | `StatefulSet` \| `Deployment` | Durable (PVCs) or cache (no PVCs). |
| `spec.pause` | bool | Pause reconciliation / scale workloads to zero. |

```yaml
mode: cluster
shards: 3
replicas: 2
workloadType: StatefulSet
```

## Image

```yaml
image: "percona/percona-valkey:{{ valkeydefaultrecommended }}"
imagePullSecrets: []
```

The engine image must be part of a compatible set with the backup and exporter images
(no version negotiation). See [Versions compatibility](versions.md).

## Resources and persistence

```yaml
resources:
  requests: { cpu: "1", memory: 2Gi }
  limits:   { cpu: "2", memory: 2Gi }

persistence:
  enabled: true
  size: 20Gi
  storageClassName: ""      # immutable once set
  reclaimPolicy: Retain
```

Disable persistence for cache-mode (`workloadType: Deployment`, no PVCs).

## Engine configuration

Live-settable engine config is applied without a pod roll where the key allows it
(`CONFIG SET`); restart-required keys trigger a rolling restart via a config-hash annotation.

```yaml
config:
  maxmemory: "1gb"
  maxmemory-policy: "allkeys-lru"
  appendonly: "no"
```

## TLS

TLS in transit is provided either by an existing Secret or by cert-manager (mutually
exclusive).

=== "Secret reference"

    ```yaml
    tls:
      enabled: true
      secretName: my-valkey-tls
    ```

=== "cert-manager"

    ```yaml
    tls:
      enabled: true
      certManager:
        enabled: true
        issuerRef:
          name: my-issuer
          kind: ClusterIssuer    # Issuer | ClusterIssuer
    ```

Requires cert-manager {{ certmanagerrecommended }}+.

## ACL users

Users map to `valkey.io/acl` Secrets; passwords come from a referenced Secret and are never
inlined. Multiple keys enable password rotation.

```yaml
users:
  - name: app
    enabled: true
    passwordSecret:
      name: app-users
      keys: [app-current, app-previous]
    commands:
      allow: ["@read", "@write"]
      deny:  ["@admin"]
    keys:
      readWrite: ["app:*"]
```

## Exporter and monitoring

```yaml
exporter:
  enabled: true
  image: "percona/valkey-exporter:{{ exporterrecommended }}"
```

The operator wires a Prometheus exporter sidecar and a PodMonitor/ServiceMonitor.

## PodDisruptionBudget

```yaml
podDisruptionBudget: Managed   # Managed | None
```

`Managed` lets the operator maintain a PDB sized to the topology; `None` disables it.

## Backup

See [Backup and restore](backup-restore.md) for the full storage/schedule surface.

```yaml
backup:
  enabled: true
  image: "percona/valkey-backup:{{ backuprecommended }}"
  storages:
    s3-primary:
      type: s3
      s3:
        bucket: percona-valkey-backups
        region: eu-central-1
        credentialsSecret: prod-s3-creds
  schedule:
    - name: nightly-full
      schedule: "0 2 * * *"
      storageName: s3-primary
      keep: 7
      type: full
```

## Upgrade options

See [Upgrades](upgrades.md) for how the version service resolves engine upgrades.

```yaml
upgradeOptions:
  apply: Disabled            # Disabled | Recommended | Latest | <version>
  schedule: "0 4 * * *"
  versionServiceEndpoint: https://check.percona.com
```

## `crVersion`

`spec.crVersion` is the operator's `major.minor` and gates CR API compatibility. Leave it
empty — the operator auto-stamps it from the operator version on first reconcile. Do not
hand-set it to a full semver; see [Upgrades](upgrades.md).
