# Percona Valkey Operator — Scenarios & Verification Guide

A hands-on cookbook of everything you can do with the operator, each with a manifest,
the command to apply it, and **how to verify it worked** (expected output included).

The verification commands here have been exercised on a local `kind` cluster. Adapt the
namespace, names, storage, and image tags to your environment.

---

## Conventions used throughout

```bash
export NS=valkey            # namespace
export CL=cache             # PerconaValkeyCluster name (short kind: pvk)
```

**Container & ports.** The Valkey container in each pod is named `server`; client port `6379`,
cluster bus `16379`, metrics `9121`.

**Where the passwords live** (auth is ON by default):

| Purpose | Secret | Key(s) |
|---------|--------|--------|
| Orchestration user `_operator` (run `CLUSTER`/`INFO`/`CONFIG`) | `internal-<cluster>-system-passwords` | `_operator`, `_backup`, `_exporter` |
| Default user / `requirepass` (read/write data) | `<cluster>-users` (or your `spec.auth.passwordSecret.name`) | `password` (or your first `keys[]`) |

Helper snippets (reused below):

```bash
# admin/topology commands run as _operator (orchestration-only: CLUSTER/INFO/CONFIG/PING)
OPPW() { kubectl -n "$NS" get secret "internal-$CL-system-passwords" -o jsonpath='{.data._operator}' | base64 -d; }
# data commands run as the default user (requirepass)
DATAPW() { kubectl -n "$NS" get secret "$CL-users" -o jsonpath='{.data.password}' | base64 -d; }
# a pod to exec into (first node)
POD() { kubectl -n "$NS" get pod -l "valkey.percona.com/cluster=$CL" -o jsonpath='{.items[0].metadata.name}'; }

# run an admin command:  vadmin CLUSTER INFO
vadmin() { kubectl -n "$NS" exec "$(POD)" -c server -- valkey-cli --user _operator -a "$(OPPW)" --no-auth-warning "$@"; }
# run a data command (cluster-aware redirects): vdata -c SET k v
vdata()  { kubectl -n "$NS" exec "$(POD)" -c server -- valkey-cli -a "$(DATAPW)" --no-auth-warning "$@"; }
```

> `_operator` is least-privilege orchestration-only — it **cannot** read the keyspace. Use the
> default user (or a user-defined ACL user) for `GET`/`SET`. This separation is intentional.

---

# Part A — Core lifecycle

## 1. Deploy a cluster

**Goal:** stand up a sharded Valkey cluster.

`cache` (ephemeral, Deployment-backed, no PVCs):

```yaml
apiVersion: valkey.percona.com/v1alpha1
kind: PerconaValkeyCluster
metadata: { name: cache, namespace: valkey }
spec:
  mode: cluster
  shards: 3
  replicas: 1
  workloadType: Deployment      # no persistence
```

`prod` (persistent, StatefulSet-backed) — see the full sample at
`config/samples/valkey_v1alpha1_perconavalkeycluster.yaml`.

```bash
kubectl create ns "$NS"
kubectl apply -f cache.yaml
```

**Verify:**

```bash
# Wait for the operator to report Ready
kubectl -n "$NS" wait --for=condition=Ready "pvk/$CL" --timeout=300s

kubectl -n "$NS" get pvk "$CL" -o wide
# NAME    STATE   REASON           SHARDS   READY   HOST                          AGE
# cache   Ready   ClusterHealthy   3        3       valkey-cache.valkey.svc       ...

# Engine view: 16384 slots assigned, 3 masters + 3 replicas
vadmin CLUSTER INFO | grep -E 'cluster_state|cluster_slots_assigned|cluster_size'
# cluster_state:ok
# cluster_slots_assigned:16384
# cluster_size:3
vadmin CLUSTER NODES        # 3 master lines (slot ranges) + 3 slave lines
```

The internal per-pod CR (`ValkeyNode`, short `vkn`) shows role/readiness:

```bash
kubectl -n "$NS" get vkn -l "valkey.percona.com/cluster=$CL"
# NAME          READY   ROLE      POD                  IP          AGE
# cache-0-0     true    primary   valkey-cache-0-0-0   10.x.x.x    ...
# cache-0-1     true    replica   ...
```

## 2. Verify cluster health (conditions)

```bash
kubectl -n "$NS" get pvk "$CL" -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
# Ready=True (ClusterHealthy)
# ClusterFormed=True (ClusterHealthy)
# SlotsAssigned=True (ClusterHealthy)
# Progressing=False (ClusterHealthy)
# Degraded=False (ClusterHealthy)
```

See **Appendix A** for every condition/reason and what it means.

## 3. Connect and read/write data

Cluster mode uses hash-slot redirects, so use `-c`:

```bash
vdata -c SET user:1 alice
vdata -c GET user:1            # -> "alice" (may MOVED-redirect to the owning shard)
vdata -c CLUSTER KEYSLOT user:1
```

**Verify** a key written through one node is readable through another (redirect works) and from
the owning shard's replica (replication serves reads).

## 4. Authentication

Auth is **on by default**. Verify it is actually enforced:

```bash
# Unauthenticated command is rejected
kubectl -n "$NS" exec "$(POD)" -c server -- valkey-cli PING
# (error) NOAUTH Authentication required.

# With the password it succeeds
vdata PING        # PONG
```

To bring your own password:

```yaml
spec:
  auth:
    enabled: true
    passwordSecret: { name: my-valkey-auth }   # key: password
```

To **disable** auth (not recommended): `spec.auth.enabled: false` — verify `PING` returns `PONG`
without credentials.

## 5. Scale shards (out and in)

```bash
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"shards":4}}'
```

**Verify scale-out** (slots rebalance onto the new shard, no data loss):

```bash
kubectl -n "$NS" get pvk "$CL" -o wide          # SHARDS 4, eventually READY 4
vadmin CLUSTER INFO | grep -E 'cluster_slots_assigned|cluster_size'
# cluster_slots_assigned:16384   (still fully covered)
# cluster_size:4
```

Scale back in — the operator drains the excess shard's slots, then deletes and `FORGET`s it:

```bash
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"shards":3}}'
# Verify: cluster_size back to 3, 16384 slots still covered, prior keys still GET-able.
```

While rebalancing, `STATE` shows `Progressing` with reason `RebalancingSlots`.

## 6. Scale replicas

```bash
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"replicas":2}}'
```

**Verify:** each shard gains a replica and every replica's link is up:

```bash
kubectl -n "$NS" get vkn -l "valkey.percona.com/cluster=$CL"   # 3 primary + 6 replica
# On a replica pod:
kubectl -n "$NS" exec valkey-$CL-0-1-0 -c server -- valkey-cli --user _operator -a "$(OPPW)" \
  --no-auth-warning INFO replication | grep -E 'role|master_link_status'
# role:slave
# master_link_status:up
```

## 7. Persistence

With `spec.persistence` set (and `workloadType: StatefulSet`), each node gets a PVC:

```yaml
spec:
  workloadType: StatefulSet
  persistence:
    size: 50Gi
    storageClassName: fast-ssd     # immutable once set
    reclaimPolicy: Retain          # Retain keeps PVCs after cluster delete
```

**Verify:**

```bash
kubectl -n "$NS" get pvc -l "valkey.percona.com/cluster=$CL"      # one Bound PVC per node
# Data survives a pod restart:
vdata -c SET persist:check ok
kubectl -n "$NS" delete pod "$(POD)"
kubectl -n "$NS" wait --for=condition=Ready "pvk/$CL" --timeout=300s
vdata -c GET persist:check        # -> "ok"
```

---

# Part B — Upgrades & resilience

## 8. Manual rolling upgrade

Change the engine image; the operator performs a **failover-aware** roll.

```bash
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"image":"percona/valkey:9.1"}}'
```

**Verify** the safe ordering and zero data loss:

```bash
kubectl -n "$NS" get events --field-selector involvedObject.name="$CL" | grep -iE 'failover|roll'
# replicas roll first; a proactive CLUSTER FAILOVER precedes rolling each live primary
kubectl -n "$NS" get pods -l "valkey.percona.com/cluster=$CL" \
  -o jsonpath='{range .items[*]}{.spec.containers[0].image}{"\n"}{end}' | sort -u   # all at the new tag
vadmin CLUSTER INFO | grep cluster_state    # cluster_state:ok throughout
```

`STATE` shows `Progressing` / `UpdatingNodes` during the roll.

## 9. Smart update via the version service

Let the operator pick & apply a recommended engine version on a schedule:

```yaml
spec:
  upgradeOptions:
    apply: Recommended                 # Disabled | Recommended | Latest | <pinned>
    schedule: "0 4 * * *"
    versionServiceEndpoint: https://check.percona.com
```

**Verify:**

```bash
kubectl -n "$NS" get events --field-selector involvedObject.name="$CL" | grep -iE 'version|EnginePin'
# NewEnginePinResolved when a new version is selected; the roll then follows scenario 8.
```

Upgrades are **gated** — watch for these reasons if a roll is held back:
`UpgradeGatedBackupRunning`, `UpgradeGatedNotReady`, `UpgradeGatedSlotsIncomplete`,
`UpgradeGatedReplicasUnsynced`, `UnsupportedDowngrade`.

## 10. Automatic failover / self-healing

```bash
# Kill a primary pod
PRIMARY=$(kubectl -n "$NS" get vkn -l "valkey.percona.com/cluster=$CL" \
  -o jsonpath='{range .items[?(@.status.role=="primary")]}{.status.pod}{"\n"}{end}' | head -1)
kubectl -n "$NS" delete pod "$PRIMARY"
```

**Verify:** a replica is promoted, the cluster returns to Ready, and the returning pod rejoins as a
replica:

```bash
kubectl -n "$NS" wait --for=condition=Ready "pvk/$CL" --timeout=300s
vadmin CLUSTER INFO | grep cluster_state          # cluster_state:ok
vadmin CLUSTER NODES                               # still 3 masters; roles may have swapped
```

## 11. Gossip repair after a mass restart

If **all** pods restart and change IPs at once (e.g. a node drain), the operator detects the
stale-gossip partition and re-MEETs every node at its current IP.

```bash
kubectl -n "$NS" delete pod -l "valkey.percona.com/cluster=$CL"   # all at once
```

**Verify** autonomous re-convergence (no manual `CLUSTER MEET` needed):

```bash
# Briefly cluster_state:fail, then the operator emits a gossip-repair MEET batch:
kubectl -n "$NS" get events --field-selector involvedObject.name="$CL" | grep -i 'ClusterMeet'
kubectl -n "$NS" wait --for=condition=Ready "pvk/$CL" --timeout=300s
vadmin CLUSTER INFO | grep cluster_state          # back to cluster_state:ok
```

A healthy cluster never re-MEETs (no churn).

## 12. Pause / resume

```bash
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"pause":true}}'    # scale workloads to 0
# ... resume:
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"pause":false}}'
```

**Verify:** paused → pods scale to 0 (StatefulSet/Deployment replicas 0), CR persists; resume →
pods return and the cluster re-forms to Ready (PVCs preserved if persistent).

---

# Part C — Data protection

## 13. On-demand backup

Declare storage on the cluster, then create a backup CR.

```yaml
# on the cluster:
spec:
  backup:
    image: perconalab/valkey-backup:main
    storages:
      s3-primary:
        type: s3
        s3:
          bucket: percona-valkey-backups
          prefix: prod
          region: eu-central-1
          credentialsSecret: prod-s3-creds     # keys: AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
---
apiVersion: valkey.percona.com/v1alpha1
kind: PerconaValkeyBackup
metadata: { name: backup-now, namespace: valkey }
spec:
  clusterName: cache
  storageName: s3-primary
  type: full
```

For local testing, an in-cluster **MinIO** works as the S3 endpoint (set `s3.endpointUrl`, e.g.
`http://minio.minio.svc:9000`).

**Verify:**

```bash
kubectl -n "$NS" get pvk-backup backup-now -o wide
# NAME         CLUSTER   STORAGE      STATE       COVERAGE   DESTINATION             COMPLETED   AGE
# backup-now   cache     s3-primary   Succeeded   complete   s3://.../prod/...       ...
# COVERAGE=complete means every one of the 16384 slots' shards was snapshotted.
```

Confirm the RDB objects + `manifest.json` landed in the bucket (e.g. `aws s3 ls`/`mc ls`).

## 14. Scheduled backups + retention

```yaml
spec:
  backup:
    storages: { ... }
    schedule:
      - name: nightly-full
        schedule: "0 2 * * *"
        storageName: s3-primary
        keep: 7              # retention: keep 7 most recent
        type: full
```

**Verify:** a `PerconaValkeyBackup` is auto-created on schedule; older ones beyond `keep` are
garbage-collected.

```bash
kubectl -n "$NS" get pvk-backup -l "valkey.percona.com/cluster=$CL" --sort-by=.metadata.creationTimestamp
```

## 15. Restore

```yaml
apiVersion: valkey.percona.com/v1alpha1
kind: PerconaValkeyRestore
metadata: { name: restore-1, namespace: valkey }
spec:
  clusterName: cache-restored      # target cluster (NewCluster strategy rebuilds it)
  backupName: backup-now           # XOR backupSource (restore from an external location)
  strategy: NewCluster
```

**Verify:** the restore reaches `Succeeded`, the target cluster forms with full slot coverage, and
the data from the backup is present:

```bash
kubectl -n "$NS" get pvk-restore restore-1 -o wide     # STATE Succeeded
kubectl -n "$NS" wait --for=condition=Ready pvk/cache-restored --timeout=600s
CL=cache-restored vdata -c GET user:1                  # -> the value that existed at backup time
```

---

# Part D — Security

## 16. TLS

**cert-manager mode (recommended)** — the operator provisions an auto-rotated Certificate:

```yaml
spec:
  tls:
    certManager:
      issuerRef: { name: my-issuer }    # a cert-manager Issuer/ClusterIssuer
```

**secret-ref mode** — bring your own Secret with `ca.crt`/`tls.crt`/`tls.key`:

```yaml
spec:
  tls:
    secretName: my-valkey-tls
```

**Verify:**

```bash
# cert-manager mode: the Certificate is issued
kubectl -n "$NS" get certificate     # READY=True for internal-<cluster>-tls
# Engine serves on TLS only (plaintext port disabled):
kubectl -n "$NS" exec "$(POD)" -c server -- valkey-cli -p 6379 PING                 # I/O error (plaintext refused)
kubectl -n "$NS" exec "$(POD)" -c server -- valkey-cli --tls \
  --cacert /etc/valkey/tls/ca.crt -a "$(DATAPW)" --no-auth-warning PING             # PONG
vadmin --tls --cacert /etc/valkey/tls/ca.crt INFO replication | grep master_link_status   # up (replication over TLS)
```

## 17. User-defined ACLs

```yaml
spec:
  users:
    - name: app
      enabled: true
      passwordSecret: { name: app-pw, keys: [app-current, app-previous] }  # rotation
      commands:
        allow: ["@read", "@write"]
        deny:  ["@admin", "flushall", "flushdb"]
      keys:
        readWrite: ["app:*"]      # also: readOnly / readWrite patterns
```

**Verify** the user is scoped correctly:

```bash
APW=$(kubectl -n "$NS" get secret app-pw -o jsonpath='{.data.app-current}' | base64 -d)
A() { kubectl -n "$NS" exec "$(POD)" -c server -- valkey-cli -c --user app -a "$APW" --no-auth-warning "$@"; }
A SET app:1 ok          # OK         (allowed key + command)
A GET app:1             # "ok"
A SET other:1 nope      # (error) NOPERM ... no permissions to access one of the keys
A FLUSHALL              # (error) NOPERM ... has no permissions to run the 'flushall' command
```

System users (`_operator`/`_backup`/`_exporter`) are reserved; names starting with `_` are rejected
at admission.

## 18. Live config vs roll-triggering config

`spec.config` keys on the **live-settable allowlist** are applied via `CONFIG SET` with **no pod
roll**; all other keys change the config hash and trigger a rolling restart.

```yaml
spec:
  config:
    maxmemory: "3gb"                 # live (no roll)
    maxmemory-policy: "allkeys-lru"  # live (no roll)
    maxclients: "20000"              # live (no roll)
    appendonly: "yes"                # NOT live -> rolling restart
```

**Verify (live key, no roll):**

```bash
RV1=$(kubectl -n "$NS" get pods -l valkey.percona.com/cluster=$CL -o jsonpath='{.items[0].metadata.resourceVersion}')
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"config":{"maxmemory":"4gb"}}}'
vadmin CONFIG GET maxmemory          # 4gb, applied live
# Pods were NOT recreated (same pod, LiveConfigApplied condition on the ValkeyNode):
kubectl -n "$NS" get vkn -l valkey.percona.com/cluster=$CL \
  -o jsonpath='{range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="LiveConfigApplied")].status}{"\n"}{end}'
```

---

# Part E — Operations & observability

## 19. Observability

**Engine metrics exporter** (sidecar, port 9121) + a Prometheus `PodMonitor`:

```yaml
spec:
  exporter:
    enabled: true              # default true
    scrapeInterval: "20s"
    tls: { enabled: false }    # set true to scrape over HTTPS
```

**Verify the engine exporter:**

```bash
kubectl -n "$NS" port-forward "$(POD)" 9121:9121 &
curl -s localhost:9121/metrics | grep -E '^valkey_up|^valkey_connected_clients'
kubectl get podmonitor -n "$NS"     # the operator-managed PodMonitor for Prometheus scrape
```

**Operator metrics** — the controller exposes its own `/metrics` (controller-runtime reconcile /
workqueue metrics, plus `valkey_operator_*` business metrics — cluster readiness, shards,
backup/restore/failover counters):

```bash
kubectl -n "$NS" port-forward deploy/valkey-operator 8080:8080 &
curl -s localhost:8080/metrics | grep -E 'controller_runtime_reconcile_total|valkey_operator_'
```

## 20. Expose externally

```yaml
spec:
  expose:
    type: LoadBalancer                       # ClusterIP (default) | NodePort | LoadBalancer
    loadBalancerSourceRanges: ["10.0.0.0/8"]
    # perPod: true                           # per-pod external addressing for cluster-mode clients
```

**Verify:**

```bash
kubectl -n "$NS" get svc -l "valkey.percona.com/cluster=$CL"   # EXTERNAL-IP populated for LoadBalancer
```

> `expose.perPod` (per-pod `cluster-announce-ip`) requires a real cloud LoadBalancer to follow
> cluster-mode redirects externally; it is not exercisable on a single-node `kind`.

## 21. PodDisruptionBudget

```yaml
spec:
  podDisruptionBudget: Managed     # Managed (operator-owned PDB) | Disabled
```

**Verify:**

```bash
kubectl -n "$NS" get pdb -l "valkey.percona.com/cluster=$CL"
# maxUnavailable is 1 cluster-wide so a voluntary drain never takes down a quorum.
```

## 22. NetworkPolicy

```yaml
spec:
  networkPolicy:
    enabled: true
    # restricts client 6379 / bus 16379 / metrics 9121 to the right peers
```

**Verify:**

```bash
kubectl -n "$NS" get networkpolicy -l "valkey.percona.com/cluster=$CL"
# Ingress: intra-cluster bus among the cluster's pods; metrics 9121 from the monitoring namespace.
```

## 23. Scheduling

```yaml
spec:
  nodeSelector: { disktype: ssd }
  tolerations: [ { key: dedicated, operator: Equal, value: valkey, effect: NoSchedule } ]
  affinity: { podAntiAffinity: { ... } }
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: DoNotSchedule
      labelSelector: { matchLabels: { app.kubernetes.io/instance: cache } }
```

**Verify:** `kubectl -n "$NS" get pods -l "valkey.percona.com/cluster=$CL" -o wide` — pods land on
the intended nodes/zones; anti-affinity keeps a shard's primary and replica off the same node.

## 24. Delete & cleanup

```bash
kubectl -n "$NS" delete pvk "$CL"
```

**Verify** the finalizer cleans up owned resources (StatefulSets/Services/Secrets/PodMonitor, and
operator-issued TLS material). PVCs are kept when `persistence.reclaimPolicy: Retain` and deleted
when `Delete`:

```bash
kubectl -n "$NS" get sts,deploy,svc,secret,pdb,networkpolicy -l "valkey.percona.com/cluster=$CL"  # gone
kubectl -n "$NS" get pvc -l "valkey.percona.com/cluster=$CL"   # retained or gone per reclaimPolicy
```

---

# Appendix A — Status conditions & reasons

`kubectl get pvk <name> -o wide` surfaces `STATE` (a coarse phase) and `REASON`. Full conditions are
in `.status.conditions`.

**Conditions:** `Ready`, `ClusterFormed`, `SlotsAssigned`, `Progressing`, `Degraded`.

**Reasons (selected):**

| Reason | Meaning |
|--------|---------|
| `ClusterHealthy` | All shards/replicas present, 16384 slots assigned, links up. |
| `Initializing` / `AddingNodes` / `UpdatingNodes` | Normal progress phases. |
| `RebalancingSlots` | Slot migration in progress (scale up/down). |
| `MissingShards` | Fewer slot-owning shards than `spec.shards`. |
| `MissingReplicas` | A shard is short of `1+replicas` ready nodes. |
| `SlotsUnassigned` | Not all 16384 slots are covered. |
| `ReplicationNotInSync` | A replica's `master_link_status` is not `up`. |
| `UsersAclError` | The rendered ACL could not be applied. |
| `ServiceError` / `ConfigMapError` / `NetworkPolicyError` / `ExposeError` | A managed resource failed to reconcile. |
| `UpgradeGated*` | A smart-update is held (backup running / not ready / slots incomplete / replicas unsynced). |
| `VersionCheckFailed` / `VersionCheckDisabled` / `NewEnginePinResolved` | Version-service outcomes. |

# Appendix B — Quick reference

```bash
kubectl -n "$NS" get pvk,vkn,pvk-backup,pvk-restore        # all CRs at a glance
kubectl -n "$NS" describe pvk "$CL"                        # events + conditions
kubectl -n "$NS" logs deploy/valkey-operator -f            # operator logs
vadmin CLUSTER INFO ; vadmin CLUSTER NODES                 # engine topology (as _operator)
vadmin INFO replication                                    # roles + master_link_status
```

# Appendix C — Troubleshooting

- **Stuck `MissingReplicas` / `ReplicationNotInSync`** → check `INFO replication` on the replica;
  `master_link_status:down` usually means an auth/`masterauth` mismatch — confirm the rendered
  `default` user accepts `requirepass`.
- **`UsersAclError`** → an in-place ACL reload was rejected (e.g. a running ACL predating a new
  grant); the change applies on the next pod restart. Check operator logs.
- **`cluster_state:fail` after a mass restart** → the operator's gossip-repair re-MEET (scenario 11)
  should recover it autonomously; if not, roll one pod at a time.
- **Exporter sidecar but no metrics** → confirm `spec.exporter.enabled` and that the `_exporter`
  ACL user exists (`internal-<cluster>-system-passwords` key `_exporter`).
- **Backup `COVERAGE` not `complete`** → a shard primary was unreachable at snapshot time; re-run
  once `cluster_state:ok`.
