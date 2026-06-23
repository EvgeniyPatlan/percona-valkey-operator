# Percona Valkey Operator — A Beginner's Guide (Scenarios & How to Verify Them)

This guide walks you through **everything the operator can do**, one task at a time. No prior
Valkey knowledge needed. Every task follows the same simple shape:

> **What it does** → **Why you'd want it** → **Do this** (copy‑paste) → **✅ How you know it worked** → **⚠️ If it didn't**

If you can run `kubectl`, you can follow this guide.

---

## Table of contents

- [Part 0 — The basics (read this first)](#part-0--the-basics-read-this-first)
- [Part 1 — One‑time setup](#part-1--onetime-setup)
- [Part 2 — Your first cluster (the happy path)](#part-2--your-first-cluster-the-happy-path)
- [Part 3 — Everyday tasks](#part-3--everyday-tasks) (scale, upgrade, config, pause)
- [Part 4 — Don't lose your data](#part-4--dont-lose-your-data) (persistence, backup, restore)
- [Part 5 — Keep it secure](#part-5--keep-it-secure) (auth, TLS, ACL users, network policy)
- [Part 6 — When things go wrong](#part-6--when-things-go-wrong) (failover, self‑healing, reading status)
- [Part 7 — Going to production](#part-7--going-to-production) (expose, PDB, scheduling, monitoring, cleanup)
- [Appendix A — Glossary](#appendix-a--glossary)
- [Appendix B — Status reasons](#appendix-b--status-reasons-what-the-reason-column-means)
- [Appendix C — Cheat sheet](#appendix-c--cheat-sheet)

---

## Part 0 — The basics (read this first)

### What is Valkey?
**Valkey** is a very fast in‑memory key/value database (a community fork of Redis). You store
values under keys (`SET user:1 alice`) and read them back (`GET user:1`). It's used for caching,
sessions, queues, and similar fast‑data workloads.

### What is "the operator"?
Running a Valkey **cluster** on Kubernetes by hand is fiddly — many pods, networking, failover,
upgrades, backups. **The operator is a robot database admin**: you describe the cluster you want in
a short YAML file, and the operator creates and babysits everything to match. If a pod dies, it
heals it. If you change the YAML, it reconciles reality to your wish.

You talk to the operator by creating **Custom Resources (CRs)** — Kubernetes objects this operator
understands:

| CR (kind) | Short name | What it is |
|-----------|-----------|------------|
| `PerconaValkeyCluster` | `pvk` | **The main one** — your Valkey cluster. |
| `PerconaValkeyBackup` | `pvk-backup` | A request to back up a cluster. |
| `PerconaValkeyRestore` | `pvk-restore` | A request to restore from a backup. |
| `ValkeyNode` | `vkn` | Internal — one per pod. The operator manages these for you; you rarely touch them. |

### Words you'll see a lot
- **Pod** — one running container (here, one Valkey server). A cluster has several.
- **Namespace** — a folder in Kubernetes where your objects live (we'll use `valkey`).
- **Shard** — a group of pods that owns a *slice* of your data. More shards = more capacity.
- **Primary** (a.k.a. master) — the pod in a shard that takes writes.
- **Replica** (a.k.a. slave) — a copy that follows a primary; takes over if the primary dies.
- **Slot** — Valkey splits all keys into **16384 slots**; each shard owns a range of them. "All
  16384 slots assigned" means the cluster is complete.
- **Failover** — promoting a replica to primary when a primary is lost.
- **`requirepass` / auth** — the password to talk to the database.
- **ACL user** — a named login with limited powers (e.g. can read but not delete).

### What a 3‑shard, 1‑replica cluster looks like
```
            cluster "cache"  (16384 slots split 3 ways)
   ┌─────────────────┬─────────────────┬─────────────────┐
   │ shard 0         │ shard 1         │ shard 2         │
   │ slots 0–5460    │ slots 5461–10922│ slots 10923–16383│
   │ primary  ◄─────┐│ primary  ◄─────┐│ primary  ◄─────┐│
   │ replica ───────┘│ replica ───────┘│ replica ───────┘│
   └─────────────────┴─────────────────┴─────────────────┘
   = 6 pods total (3 primaries + 3 replicas)
```

💡 **Rule of thumb:** total pods = `shards × (1 + replicas)`.

---

## Part 1 — One‑time setup

### What you need
- `kubectl` installed and pointed at a Kubernetes cluster.
- That's it for a test run. For a real run you also want a storage class (for persistence) and,
  optionally, cert‑manager (for TLS) and Prometheus (for metrics).

### (Optional) Get a throwaway test cluster with kind
If you don't have a cluster, [`kind`](https://kind.sigs.k8s.io/) runs one inside Docker:

```bash
kind create cluster --name valkey-test
```

### Install the operator
Apply the **cluster-wide** install manifest (operator + CRDs + RBAC). It watches
**all** namespaces, so the `valkey`-namespace examples throughout this guide work:

```bash
kubectl apply --server-side -f deploy/cw-bundle.yaml
```

> Prefer least privilege? `deploy/bundle.yaml` installs a **namespaced** operator that
> watches only its own `valkey-operator` namespace — then create your clusters in
> `valkey-operator` (not `valkey`).

**✅ How you know it worked:** the operator pod is `Running`:

```bash
kubectl get pods -n valkey-operator     # or wherever the bundle installs it
# NAME                              READY   STATUS    RESTARTS   AGE
# valkey-operator-xxxxxxxxx-xxxxx   1/1     Running   0          30s
```

And the four CRDs are installed:

```bash
kubectl get crds | grep valkey.percona.com
# perconavalkeyclusters.valkey.percona.com
# perconavalkeybackups.valkey.percona.com
# perconavalkeyrestores.valkey.percona.com
# valkeynodes.valkey.percona.com
```

⚠️ **If the operator isn't Running:** `kubectl describe pod -n valkey-operator <pod>` and
`kubectl logs -n valkey-operator deploy/valkey-operator` will tell you why (usually image pull or
RBAC).

### Two shortcuts we'll use everywhere
Pick a namespace and a cluster name once:

```bash
export NS=valkey       # the namespace we'll work in
export CL=cache        # the name of the cluster we'll create
kubectl create namespace "$NS"
```

The Valkey container inside each pod is named **`server`**. Ports: **6379** (clients), 16379
(internal cluster chatter), 9121 (metrics).

**Passwords are created for you automatically** (the database is password‑protected by default).
There are two you'll use:

| To do this... | Use this login | Password is in Secret... | ...under key |
|---|---|---|---|
| Admin/health (`CLUSTER INFO`, `INFO`) | `_operator` | `internal-<cluster>-system-passwords` | `_operator` |
| Read/write data (`GET`/`SET`) | default user | `<cluster>-users` | `password` |

💡 **Paste these helper functions once** — the rest of the guide uses them so you don't retype long
commands. (They just fetch the password and run `valkey-cli` inside a pod.)

```bash
# password for the admin user:
OPPW() { kubectl -n "$NS" get secret "internal-$CL-system-passwords" -o jsonpath='{.data._operator}' | base64 -d; }
# password for reading/writing data:
DATAPW() { kubectl -n "$NS" get secret "$CL-users" -o jsonpath='{.data.password}' | base64 -d; }
# name of the first pod in the cluster:
POD() { kubectl -n "$NS" get pod -l "valkey.percona.com/cluster=$CL" -o jsonpath='{.items[0].metadata.name}'; }

# run an ADMIN command, e.g.:  vadmin CLUSTER INFO
vadmin() { kubectl -n "$NS" exec "$(POD)" -c server -- valkey-cli --user _operator -a "$(OPPW)" --no-auth-warning "$@"; }
# run a DATA command, e.g.:     vdata -c SET k v
vdata()  { kubectl -n "$NS" exec "$(POD)" -c server -- valkey-cli -a "$(DATAPW)" --no-auth-warning "$@"; }
```

💡 **Why two logins?** The `_operator` login is locked down to cluster management only — for safety
it **cannot read your data**. So use `vadmin` for health/topology and `vdata` for keys. (This split
is a deliberate security feature.)

---

## Part 2 — Your first cluster (the happy path)

**Goal:** get a working 3‑shard cluster and store a value in it. ~5 minutes.

### Step 1 — Write the cluster file
Save this as `cache.yaml`:

```yaml
apiVersion: valkey.percona.com/v1alpha1
kind: PerconaValkeyCluster
metadata:
  name: cache
  namespace: valkey
spec:
  mode: cluster        # a sharded cluster (vs a single replicated group)
  shards: 3            # 3 slices of data
  replicas: 1          # 1 backup copy per shard
  workloadType: Deployment   # "Deployment" = no disk (cache). Use "StatefulSet" for persistence.
```

### Step 2 — Apply it
```bash
kubectl apply -f cache.yaml
```

### Step 3 — Wait for it to be ready
```bash
kubectl -n "$NS" wait --for=condition=Ready "pvk/$CL" --timeout=300s
```

**✅ How you know it worked:**
```bash
kubectl -n "$NS" get pvk "$CL" -o wide
# NAME    STATE   REASON           SHARDS   READY   HOST                       AGE
# cache   Ready   ClusterHealthy   3        3       valkey-cache.valkey.svc    2m
```
`STATE = Ready` and `READY = 3` (3 of 3 shards healthy) means you're done. The `HOST` column is the
address your apps connect to.

Double‑check the database itself agrees:
```bash
vadmin CLUSTER INFO | grep -E 'cluster_state|cluster_slots_assigned|cluster_size'
# cluster_state:ok               <- the cluster is healthy
# cluster_slots_assigned:16384   <- all data slots are covered
# cluster_size:3                 <- 3 shards
```

### Step 4 — Store and read a value
Because data is spread across shards, add `-c` so the client follows redirects automatically:
```bash
vdata -c SET user:1 alice
vdata -c GET user:1
# "alice"
```

🎉 **That's a working Valkey cluster.** Everything below builds on this.

⚠️ **If `STATE` is stuck on something other than `Ready`:** that's normal for the first minute
(`Initializing`, `AddingNodes`). If it stays stuck >5 min, jump to
[Part 6 — reading status](#read-the-status-the-first-thing-to-do-when-stuck) and
[Appendix B](#appendix-b--status-reasons-what-the-reason-column-means).

---

## Part 3 — Everyday tasks

### 3.1 Add capacity (scale shards)

**What it does:** adds/removes shards and moves data slots so each shard holds a fair share.
**Why:** you need more memory/throughput (scale **out**), or less (scale **in**).

**Do this** — go from 3 shards to 4:
```bash
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"shards":4}}'
```

**✅ Worked when:** the new shard appears, data slots rebalance, and nothing is lost:
```bash
kubectl -n "$NS" get pvk "$CL" -o wide               # SHARDS 4, READY 4 (after a minute)
vadmin CLUSTER INFO | grep -E 'cluster_slots_assigned|cluster_size'
# cluster_slots_assigned:16384   <- still fully covered (no gap = no data loss)
# cluster_size:4
vdata -c GET user:1                                   # "alice" — your old data is still there
```

Scale back **in** the same way (`"shards":3`). The operator safely moves slots off the doomed
shard, then removes it. During rebalancing you'll see `STATE: Progressing / RebalancingSlots`.

### 3.2 Add redundancy (scale replicas)

**What it does:** changes how many backup copies each shard keeps.
**Why:** more replicas = survive more failures + spread read load.

```bash
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"replicas":2}}'
```

**✅ Worked when:** every shard has 2 replicas and each is caught up:
```bash
kubectl -n "$NS" get vkn -l "valkey.percona.com/cluster=$CL"   # now 3 primary + 6 replica rows, all READY=true
```

### 3.3 Upgrade the Valkey version

**What it does:** changes the database image; the operator rolls pods **one shard at a time,
replicas first, and gracefully fails a primary over before touching it** — so the cluster stays up.
**Why:** security patches, new features.

```bash
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"image":"percona/valkey:9.1"}}'
```

**✅ Worked when:** every pod is on the new image and the cluster never went down:
```bash
kubectl -n "$NS" get pods -l "valkey.percona.com/cluster=$CL" \
  -o jsonpath='{range .items[*]}{.spec.containers[0].image}{"\n"}{end}' | sort -u   # all show the new tag
vadmin CLUSTER INFO | grep cluster_state             # cluster_state:ok throughout
vdata -c GET user:1                                  # data preserved
```

💡 **Hands‑off upgrades:** instead of picking the image yourself, let the operator track a
recommended version on a schedule:
```yaml
spec:
  upgradeOptions:
    apply: Recommended          # Disabled | Recommended | Latest | a pinned version
    schedule: "0 4 * * *"       # 4am daily
    versionServiceEndpoint: https://check.percona.com
```
The operator will only upgrade when it's safe; if it holds off you'll see a reason like
`UpgradeGatedBackupRunning` or `UpgradeGatedNotReady` (see [Appendix B](#appendix-b--status-reasons-what-the-reason-column-means)).

### 3.4 Change a setting (`spec.config`)

**What it does:** sets Valkey config. Some settings apply **instantly with no restart**; others need
a rolling restart. The operator figures out which.

```yaml
spec:
  config:
    maxmemory: "3gb"                 # instant, no restart
    maxmemory-policy: "allkeys-lru"  # instant, no restart
    maxclients: "20000"              # instant, no restart
    appendonly: "yes"                # needs a rolling restart
```

**✅ Worked when** (instant setting):
```bash
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"config":{"maxmemory":"4gb"}}}'
vadmin CONFIG GET maxmemory          # 4gb — and your pods were NOT restarted
```

### 3.5 Pause / resume (save money, keep the data)

**What it does:** scales the cluster to zero pods but keeps its definition and (if persistent) its
data. **Why:** dev/test environments overnight.

```bash
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"pause":true}}'    # stop
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"pause":false}}'   # start again
```

**✅ Worked when:** after pause, `kubectl -n "$NS" get pods -l valkey.percona.com/cluster=$CL`
returns nothing; after resume, the pods come back and `STATE` returns to `Ready`.

---

## Part 4 — Don't lose your data

### 4.1 Turn on persistence (survive restarts)

**What it does:** gives each pod a disk (PVC) so data survives pod restarts.
**Why:** anything that isn't a pure cache.

```yaml
spec:
  workloadType: StatefulSet     # required for disks
  persistence:
    size: 50Gi
    storageClassName: fast-ssd  # your cluster's storage class; cannot be changed later
    reclaimPolicy: Retain       # Retain = keep the disks if the cluster is deleted; Delete = remove them
```

**✅ Worked when:** data survives a pod kill:
```bash
kubectl -n "$NS" get pvc -l "valkey.percona.com/cluster=$CL"   # one Bound disk per pod
vdata -c SET keep:me yes
kubectl -n "$NS" delete pod "$(POD)"                           # kill a pod
kubectl -n "$NS" wait --for=condition=Ready "pvk/$CL" --timeout=300s
vdata -c GET keep:me                                           # "yes" — survived
```

### 4.2 Back up to cloud storage

**What it does:** snapshots every shard to S3 / Google Cloud Storage / Azure (or any S3‑compatible
store like MinIO).
**Why:** disaster recovery.

**Step 1 — tell the cluster where backups go** (add to the cluster's `spec`):
```yaml
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
          credentialsSecret: prod-s3-creds   # a Secret with AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
          # endpointUrl: http://minio.minio.svc:9000   # set this for MinIO / S3-compatible stores
```

**Step 2 — ask for a backup:**
```yaml
apiVersion: valkey.percona.com/v1alpha1
kind: PerconaValkeyBackup
metadata: { name: backup-now, namespace: valkey }
spec:
  clusterName: cache
  storageName: s3-primary    # must match a name under spec.backup.storages
  type: full
```

**✅ Worked when:** the backup reaches `Succeeded` with **complete** coverage:
```bash
kubectl -n "$NS" get pvk-backup backup-now -o wide
# NAME         CLUSTER   STORAGE      STATE       COVERAGE   DESTINATION         COMPLETED   AGE
# backup-now   cache     s3-primary   Succeeded   complete   s3://.../prod/...   ...
```
`COVERAGE = complete` means every shard was captured. Then check the bucket has the `.rdb` files +
a `manifest.json` (`aws s3 ls ...` or MinIO's `mc ls ...`).

⚠️ **If `COVERAGE` isn't `complete`:** a shard was unreachable during the snapshot — wait for
`cluster_state:ok` and re‑run.

### 4.3 Back up automatically on a schedule

```yaml
spec:
  backup:
    storages: { ... as above ... }
    schedule:
      - name: nightly-full
        schedule: "0 2 * * *"     # 2am daily (cron syntax)
        storageName: s3-primary
        keep: 7                   # keep the 7 newest, auto-delete older ones
        type: full
```
**✅ Worked when:** a new `pvk-backup` appears each night and old ones beyond `keep` disappear:
```bash
kubectl -n "$NS" get pvk-backup -l "valkey.percona.com/cluster=$CL" --sort-by=.metadata.creationTimestamp
```

### 4.4 Restore from a backup

**What it does:** rebuilds a cluster from a backup. The safest strategy, `NewCluster`, restores into
a brand‑new cluster and verifies all 16384 slots are covered before declaring success.

```yaml
apiVersion: valkey.percona.com/v1alpha1
kind: PerconaValkeyRestore
metadata: { name: restore-1, namespace: valkey }
spec:
  clusterName: cache-restored    # the NEW cluster to create
  backupName: backup-now         # which backup (or use backupSource for an external location)
  strategy: NewCluster
```

**✅ Worked when:** the restore is `Succeeded`, the new cluster is `Ready`, and your data is there:
```bash
kubectl -n "$NS" get pvk-restore restore-1 -o wide          # STATE Succeeded
kubectl -n "$NS" wait --for=condition=Ready pvk/cache-restored --timeout=600s
CL=cache-restored vdata -c GET user:1                       # the value from backup time
```

---

## Part 5 — Keep it secure

### 5.1 Authentication (it's on by default)

**What it does:** requires a password. The operator generates one and protects the cluster from the
start.

**✅ Verify it's actually enforced:**
```bash
kubectl -n "$NS" exec "$(POD)" -c server -- valkey-cli PING     # (error) NOAUTH Authentication required.
vdata PING                                                      # PONG  (with the password)
```

**Bring your own password** instead of the generated one:
```yaml
spec:
  auth:
    enabled: true
    passwordSecret: { name: my-valkey-auth }   # a Secret with key: password
```

### 5.2 Encrypt traffic with TLS

**What it does:** turns on TLS so traffic is encrypted. Two ways:

**Easiest — cert‑manager** (the operator gets and rotates the certificate for you):
```yaml
spec:
  tls:
    certManager:
      issuerRef: { name: my-issuer }   # an existing cert-manager Issuer/ClusterIssuer
```
**Bring your own certificate:**
```yaml
spec:
  tls:
    secretName: my-valkey-tls          # a Secret containing ca.crt, tls.crt, tls.key
```

**✅ Worked when:** TLS is required and plain connections are refused:
```bash
kubectl -n "$NS" get certificate     # (cert-manager mode) READY=True
kubectl -n "$NS" exec "$(POD)" -c server -- valkey-cli -p 6379 PING                  # I/O error (plaintext refused)
kubectl -n "$NS" exec "$(POD)" -c server -- valkey-cli --tls \
  --cacert /etc/valkey/tls/ca.crt -a "$(DATAPW)" --no-auth-warning PING              # PONG
```

### 5.3 Add limited‑power logins (ACL users)

**What it does:** creates named users that can only do certain things — e.g. an app user that can
read/write its own keys but can't run admin commands or wipe the database.
**Why:** least privilege; don't hand every app the master password.

```yaml
spec:
  users:
    - name: app
      enabled: true
      passwordSecret: { name: app-pw, keys: [app-current, app-previous] }  # two keys = password rotation
      commands:
        allow: ["@read", "@write"]      # may read and write
        deny:  ["@admin", "flushall", "flushdb"]   # may NOT administer or wipe
      keys:
        readWrite: ["app:*"]            # only keys starting with "app:"
```

**✅ Worked when** the user can do what it's allowed and is blocked from the rest:
```bash
APW=$(kubectl -n "$NS" get secret app-pw -o jsonpath='{.data.app-current}' | base64 -d)
A() { kubectl -n "$NS" exec "$(POD)" -c server -- valkey-cli -c --user app -a "$APW" --no-auth-warning "$@"; }
A SET app:1 ok          # OK            (allowed)
A GET app:1             # "ok"
A SET other:1 nope      # (error) NOPERM ... no permissions to access one of the keys
A FLUSHALL              # (error) NOPERM ... has no permissions to run the 'flushall' command
```
💡 Names starting with `_` (like `_operator`) are reserved for the operator and are rejected.

### 5.4 Lock down the network

```yaml
spec:
  networkPolicy:
    enabled: true     # restrict who can reach ports 6379 / 16379 / 9121
```
**✅ Worked when:** `kubectl -n "$NS" get networkpolicy -l "valkey.percona.com/cluster=$CL"` shows
the policy. Now only the cluster's own pods (and Prometheus, for metrics) can reach it.

---

## Part 6 — When things go wrong

The operator **heals itself**. These scenarios show that, and how to read what's happening.

### Read the status — the first thing to do when stuck
```bash
kubectl -n "$NS" get pvk "$CL" -o wide          # STATE + REASON columns (quick view)
kubectl -n "$NS" describe pvk "$CL"             # full conditions + recent events
kubectl -n "$NS" logs deploy/valkey-operator    # what the operator is doing/erroring on
```
The `REASON` column is your main clue — every value is explained in
[Appendix B](#appendix-b--status-reasons-what-the-reason-column-means).

### 6.1 A pod dies → automatic failover
**What happens:** if a primary pod dies, the operator promotes its replica and heals back to Ready.

```bash
# kill a primary and watch it recover:
PRIMARY=$(kubectl -n "$NS" get vkn -l "valkey.percona.com/cluster=$CL" \
  -o jsonpath='{range .items[?(@.status.role=="primary")]}{.status.pod}{"\n"}{end}' | head -1)
kubectl -n "$NS" delete pod "$PRIMARY"
```
**✅ Worked when:** it returns to Ready on its own and a replica was promoted:
```bash
kubectl -n "$NS" wait --for=condition=Ready "pvk/$CL" --timeout=300s
vadmin CLUSTER INFO | grep cluster_state        # cluster_state:ok
```

### 6.2 Everything restarts at once → gossip repair
**What happens:** if *all* pods restart together (e.g. a node drains) and get new IPs, cluster
members can lose track of each other. The operator detects this and re‑introduces them
automatically — no manual fixing.

```bash
kubectl -n "$NS" delete pod -l "valkey.percona.com/cluster=$CL"   # all at once
```
**✅ Worked when:** it heals back to Ready by itself (you may briefly see `cluster_state:fail`):
```bash
kubectl -n "$NS" wait --for=condition=Ready "pvk/$CL" --timeout=300s
vadmin CLUSTER INFO | grep cluster_state        # cluster_state:ok
```

---

## Part 7 — Going to production

### 7.1 Let apps outside the cluster connect (expose)
```yaml
spec:
  expose:
    type: LoadBalancer                       # ClusterIP (default, in-cluster only) | NodePort | LoadBalancer
    loadBalancerSourceRanges: ["10.0.0.0/8"] # optional: who may connect
```
**✅ Worked when:** `kubectl -n "$NS" get svc -l "valkey.percona.com/cluster=$CL"` shows an
`EXTERNAL-IP`.
⚠️ The `expose.perPod` option (for external **cluster‑aware** clients) needs a real cloud load
balancer; it won't work on single‑node `kind`.

### 7.2 Protect against accidental drains (PodDisruptionBudget)
```yaml
spec:
  podDisruptionBudget: Managed     # operator keeps at most 1 pod down during voluntary disruptions
```
**✅ Worked when:** `kubectl -n "$NS" get pdb -l "valkey.percona.com/cluster=$CL"` shows a budget.

### 7.3 Spread pods across nodes/zones (scheduling)
```yaml
spec:
  nodeSelector: { disktype: ssd }
  tolerations: [ { key: dedicated, operator: Equal, value: valkey, effect: NoSchedule } ]
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: DoNotSchedule
      labelSelector: { matchLabels: { app.kubernetes.io/instance: cache } }
```
**✅ Worked when:** `kubectl -n "$NS" get pods -l "valkey.percona.com/cluster=$CL" -o wide` shows
pods landing on the intended nodes/zones (and a shard's primary + replica aren't on the same node).

### 7.4 Monitoring (Prometheus)
Each pod runs a metrics exporter (port 9121) and the operator creates a `PodMonitor` for Prometheus.
```yaml
spec:
  exporter:
    enabled: true            # on by default
    scrapeInterval: "20s"
```
**✅ Worked when** you can scrape engine metrics:
```bash
kubectl -n "$NS" port-forward "$(POD)" 9121:9121 &
curl -s localhost:9121/metrics | grep -E '^valkey_up|^valkey_connected_clients'
kubectl -n "$NS" get podmonitor                 # the operator-managed scrape config
```
The operator itself also serves metrics about its own work (reconcile counts, cluster readiness,
backup/restore counters) on its `/metrics` endpoint.

### 7.5 Delete a cluster (and what's left behind)
```bash
kubectl -n "$NS" delete pvk "$CL"
```
**✅ Worked when:** all the operator‑created objects are gone:
```bash
kubectl -n "$NS" get sts,deploy,svc,secret,pdb,networkpolicy -l "valkey.percona.com/cluster=$CL"   # nothing
kubectl -n "$NS" get pvc -l "valkey.percona.com/cluster=$CL"   # KEPT if reclaimPolicy: Retain, else gone
```
💡 With `persistence.reclaimPolicy: Retain`, your data disks survive the delete — handy, but
remember to clean them up manually when you're truly done.

---

## Appendix A — Glossary

| Term | Plain meaning |
|------|---------------|
| **CR / Custom Resource** | A YAML object this operator understands (e.g. `PerconaValkeyCluster`). |
| **CRD** | The schema that teaches Kubernetes about those CRs. Installed with the operator. |
| **Reconcile** | The operator's core loop: compare your YAML to reality and fix the difference. |
| **Pod** | One running Valkey server container. |
| **Shard** | A group (primary + replicas) owning a slice of the data. |
| **Primary / Replica** | Write‑accepting pod / its read‑only follower that can be promoted. |
| **Slot** | One of 16384 buckets all keys map to; shards own slot ranges. |
| **Failover** | Promoting a replica when its primary is lost. |
| **`requirepass`** | The database password (the default user's password). |
| **ACL user** | A named login with restricted commands/keys. |
| **PVC** | A persistent disk attached to a pod. |
| **PDB** | PodDisruptionBudget — limits how many pods can be down during a voluntary drain. |

---

## Appendix B — Status reasons (what the `REASON` column means)

`kubectl get pvk <name> -o wide` shows `STATE` and `REASON`. Conditions live in `.status.conditions`
(`Ready`, `ClusterFormed`, `SlotsAssigned`, `Progressing`, `Degraded`).

| Reason | What it means | Is it a problem? |
|--------|---------------|------------------|
| `ClusterHealthy` | All good. | No — this is the goal. |
| `Initializing`, `AddingNodes`, `UpdatingNodes` | Normal work in progress. | No (transient). |
| `RebalancingSlots` | Moving data slots (scaling). | No (transient). |
| `MissingShards` | Fewer shards than you asked for. | Wait; if persistent, investigate. |
| `MissingReplicas` | A shard is short of ready replicas. | Wait; if persistent, see logs. |
| `SlotsUnassigned` | Not all 16384 slots covered yet. | Transient during setup; persistent = problem. |
| `ReplicationNotInSync` | A replica isn't caught up to its primary. | Usually transient. |
| `UsersAclError` | A live ACL/password change wasn't accepted. | Applies on next pod restart; check logs. |
| `ServiceError`, `ConfigMapError`, `NetworkPolicyError`, `ExposeError` | A Kubernetes object failed to reconcile. | Yes — `describe` / logs. |
| `UpgradeGated...` | A version upgrade is held (backup running / not ready / slots incomplete / replicas unsynced). | No — safety hold. |
| `VersionCheckFailed`, `VersionCheckDisabled`, `NewEnginePinResolved` | Version‑service outcomes. | Informational. |

---

## Appendix C — Cheat sheet

```bash
# everything about your cluster at a glance
kubectl -n "$NS" get pvk,vkn,pvk-backup,pvk-restore
kubectl -n "$NS" describe pvk "$CL"            # conditions + events
kubectl -n "$NS" logs deploy/valkey-operator -f

# health & topology (admin login)
vadmin CLUSTER INFO            # cluster_state / slots / size
vadmin CLUSTER NODES           # who owns which slots; primary/replica roles
vadmin INFO replication        # roles + master_link_status (should be "up")

# data (default login) — note the -c for cluster redirects
vdata -c SET k v
vdata -c GET k

# scale / upgrade / pause (edit the wish, operator does the rest)
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"shards":4}}'
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"replicas":2}}'
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"image":"percona/valkey:9.1"}}'
kubectl -n "$NS" patch pvk "$CL" --type=merge -p '{"spec":{"pause":true}}'
```

**The golden rule:** you change the **`spec`** (your wish); the operator makes reality match and
reports back in **`status`** (`STATE`, `REASON`, conditions). When in doubt, read the status.
