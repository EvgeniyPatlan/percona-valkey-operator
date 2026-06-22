# CRD Gap Analysis — Helm chart config surface vs. `PerconaValkeyCluster` CRD

> **Purpose.** Compare the user-facing configuration surface of the existing
> `percona-valkey` Helm chart (the *requirements spec*) against the
> `PerconaValkeyCluster` CRD we are building, and produce a prioritized list of
> fields/capabilities to **add** to the operator API so it is *not less
> configurable* than the chart — while correctly **not** flagging things that are
> deliberate architectural differences (deployment model, Sentinel, Helm Jobs).
>
> **Sources.**
> Chart: `/tmp/pvk-helm/helm/percona-valkey/values.yaml`, `values.schema.json`,
> `/tmp/pvk-helm/README.md`, `/tmp/pvk-helm/documentation.md`.
> Operator: `pkg/apis/valkey/v1alpha1/{perconavalkeycluster,shared,valkeynode}_types.go`,
> `perconavalkeycluster_defaults.go`, `docs/architecture/03-api-design.md`,
> `docs/architecture/01-decisions.md` (ADRs).
>
> **Framing lens (applied strictly).**
> 1. *Deployment-model differences are NOT gaps.* The chart orchestrates Valkey
>    with Helm Jobs (cluster-init / cluster-scale / cluster-precheck /
>    test-connection, the backup CronJob, the Sentinel StatefulSet); the operator
>    does that in its controllers. A chart Job that has no user knob is not a gap.
> 2. *Deliberate architectural choices are NOT gaps* — but are listed under
>    "Out of scope by design" with the ADR reference. The biggest one: the
>    operator uses **operator-driven failover, NO Sentinel** (ADR-007), so the
>    whole `sentinel.*` block is out-of-scope-by-design (with one carry-over noted:
>    failover *tuning* knobs).
> 3. A **gap** is only counted when the chart lets a *user configure* something
>    (a value/knob) that an operator user **cannot express** via the
>    `PerconaValkeyCluster` spec.

---

## 1. Executive summary

### 1.1 Counts

| Bucket | Count |
|--------|-------|
| Chart top-level value areas reviewed | **40** (walked every top-level key) |
| Distinct knobs/capabilities assessed | ~95 |
| **SHOULD-ADD** (operator is genuinely less configurable; security-relevant prioritized) | **14** |
| **CONSIDER** (nice-to-have, decide later) | **9** |
| **HANDLED-DIFFERENTLY** (operator covers it another way) | **18** |
| **Out of scope — by design** (ADR-backed) | **11** |
| **Already covered** (1:1 or near-1:1 in the CRD) | **18** |

> Counts are by capability, not by raw YAML leaf — e.g. the chart's `metrics.*`
> serviceMonitor/podMonitor/prometheusRule tree is one capability ("exporter
> observability wiring"), and `auth.*` is one capability ("default-user auth").

### 1.2 Top ~10 prioritized additions (the actionable shortlist)

| # | Field / capability | Why it matters | Priority |
|---|--------------------|----------------|----------|
| 1 | **`spec.auth` (default-user password / requirepass)** — `enabled`, `passwordSecret`, `nopass` | **SECURITY.** Chart's primary auth knob. The CRD only models ACL `users[]` (non-`default` users) + `_operator`/`_exporter`. There is no user-facing way to set/disable the *default* user password (`requirepass`). Without it a user cannot do the chart's most basic thing: "turn on a password." | **CRITICAL** |
| 2 | **`spec.tls` hardening knobs** — `authClients` (mTLS require/optional), `dhParamsSecret`, `ciphers`/`ciphersuites`, `disablePlaintext`/`allowPlaintextPort` | **SECURITY.** Chart exposes mutual-TLS enforcement, DH params, cipher policy, and plaintext-port control. The CRD `TLSConfig` is only `secretName` XOR `certManager` — no mTLS/cipher surface at all. | **HIGH** |
| 3 | **`spec.serviceExposure` / per-pod external access** — LoadBalancer/NodePort, annotations, sourceRanges, `externalTrafficPolicy`, per-ordinal nodePorts | Chart's `externalAccess` exposes Valkey outside the cluster (incl. cluster-mode per-pod announce-IP). The CRD has **no Service exposure surface** (only `status.host`). Real production need. | **HIGH** |
| 4 | **`spec.serviceAccountName` + `automountServiceAccountToken`** | **SECURITY.** Chart lets you BYO SA and turn off token automount (default `false`). The CRD has no pod-SA field for the data pods. | **HIGH** |
| 5 | **`spec.podSecurityContext` + `spec.containerSecurityContext`** | **SECURITY.** Chart exposes full pod/container security contexts (runAsUser, fsGroup, seccomp, caps, readOnlyRootFilesystem). The CRD has none — security context is implicit/operator-fixed. Hardening parity needs an override. | **HIGH** |
| 6 | **`spec.env` / `spec.extraEnvVars`** (extra env on the server container) | Common escape hatch (TZ, tuning, `valueFrom`). The CRD only has `containers[]` strategic-merge, which is clumsy for "just add an env var." | **MEDIUM-HIGH** |
| 7 | **`spec.disableCommands`** (rename-command / disable dangerous commands) | **SECURITY-adjacent.** Chart disables `FLUSHDB`/`FLUSHALL` by default and lets users add more. The CRD has no equivalent (can't be done via `config` map because the operator base-config wins and `rename-command` is multi-valued). | **MEDIUM-HIGH** |
| 8 | **`spec.priorityClassName`** | Scheduling priority for stateful DB pods is a standard production knob. Not in the CRD. | **MEDIUM** |
| 9 | **`spec.exporter` observability wiring** — `serviceMonitor` / `podMonitor` / `prometheusRule` toggles (interval, labels, relabelings, rules) | Chart ships full Prometheus-Operator CR generation. The CRD `ExporterSpec` is only `{enabled,image,resources}` — no ServiceMonitor/PrometheusRule. Most users expect the operator to emit these. | **MEDIUM** |
| 10 | **`spec.persistence.accessModes` + PVC retention policy on scale** | Chart exposes `accessModes`, `persistentVolumeClaimRetentionPolicy.{whenDeleted,whenScaled}`. CRD has `reclaimPolicy` (node-delete only) and a hardcoded RWO; no `whenScaled` semantics, no accessModes override. | **MEDIUM** |

Honorable mentions just below the line (all MEDIUM): `terminationGracePeriodSeconds`, `runtimeClassName`, `dnsPolicy`/`dnsConfig`, `podAnnotations`/`podLabels`, `extraVolumes`/`extraVolumeMounts`/`extraValkeySecrets`.

---

## 2. Gap tables by feature area

Legend for **Operator/CRD status**: ✅ covered · ⚠️ partial · ❌ absent.
Legend for **Classification**: `SHOULD-ADD` · `CONSIDER` · `HANDLED-DIFFERENTLY` · `BY-DESIGN` (out of scope) · `COVERED`.

### 2.1 Global / identity

| Chart value(s) | Operator/CRD status | Classification | Recommendation (field + location) | Priority |
|----------------|---------------------|----------------|-----------------------------------|----------|
| `global.imageRegistry` | ❌ (no registry-prefix knob; `spec.image` is a full ref) | HANDLED-DIFFERENTLY | Full image refs in `spec.image`/`spec.exporter.image`/`spec.backup.image` cover this; a registry prefix is a chart-templating convenience, not needed in a CRD. | LOW |
| `nameOverride` / `fullnameOverride` | ❌ | HANDLED-DIFFERENTLY | The CR's `metadata.name` *is* the cluster name; naming is `pkg/naming`-driven (ADR-003). No gap. | — |
| `commonLabels` | ❌ | CONSIDER | Add `spec.metadata.labels` / `spec.metadata.annotations` (PSMDB/PXC have `spec.metadata`) propagated to managed objects. Currently only operator-fixed labels are applied. | MEDIUM |
| `clusterDomain` (`cluster.local`) | ❌ | CONSIDER | Add `spec.clusterServiceDNSSuffix` (or `clusterDomain`). Needed for non-default DNS domains; the operator builds `status.host` from this. | MEDIUM |
| `image.pullPolicy` | ❌ (only `imagePullSecrets`) | SHOULD-ADD | Add `spec.imagePullPolicy corev1.PullPolicy` to `PerconaValkeyClusterSpec`; propagate to ValkeyNode. Trio operators all expose this. | MEDIUM |
| `image.pullSecrets` | ✅ `spec.imagePullSecrets` | COVERED | — | — |

### 2.2 Mode / topology

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `mode: standalone\|cluster\|sentinel` | ⚠️ `spec.mode: cluster\|replication\|standalone` (`sentinel` deliberately absent) | BY-DESIGN (sentinel) / COVERED (standalone, cluster) | `sentinel` mode is replaced by `replication` + operator failover (ADR-007). `replication` ≈ chart's non-Sentinel master/replica. No gap; note naming difference in docs. | — |
| `cluster.replicas` / `cluster.replicasPerPrimary` | ✅ `spec.shards` + `spec.replicas` (replicas-per-shard) | COVERED | The operator's `shards × (1+replicas)` model is the CRD-native expression of the chart's total-pods + replicas-per-primary. | — |
| `cluster.nodeTimeout` (15000) | ⚠️ only via `spec.config["cluster-node-timeout"]` — but base config *wins* and this key is in the operator-managed set (silently ignored, 03 §2.6) | SHOULD-ADD | Add `spec.clusterNodeTimeout *int32` (or a small `spec.clusterConfig` struct) the operator folds into base config. Today a user **cannot** change node-timeout at all. | MEDIUM |
| `cluster.busPort` (16379) | ❌ (operator hardcodes 16379) | CONSIDER | Bus port is conventionally fixed (client+10000). Leave fixed unless a user need surfaces. | LOW |
| `cluster.precheckBeforeScaleDown` | ❌ (Helm pre-upgrade hook Job) | HANDLED-DIFFERENTLY | The operator's reconcile owns safe scale-down (slot migration before removing a shard). The Helm precheck Job is replaced by controller logic. Not a user knob worth porting. | — |
| `standalone.useDeployment` / `standalone.strategy` | ✅ `spec.workloadType: Deployment` (cache) | COVERED | `workloadType=Deployment` is the CRD expression of cache-only/no-PVC. Deployment `strategy` is operator-managed. | — |

### 2.3 Authentication (`auth.*`) — **SECURITY**

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `auth.enabled` (default `true`) | ❌ no top-level auth toggle | **SHOULD-ADD** | Add `spec.auth.enabled *bool` (default true). Drives whether the `default` user has a password / `requirepass` is set. | **CRITICAL** |
| `auth.password` / `auth.existingSecret` | ⚠️ ACL `users[]` reference Secrets, but the **`default` user** password is unmodelled | **SHOULD-ADD** | Add `spec.auth.passwordSecret UserPasswordSecret` (Secret-ref only — never inline, per ADR-008) to set the `default` user's password (`requirepass`). This is the chart's single most-used knob and has no CRD equivalent. | **CRITICAL** |
| `auth.usePasswordFiles` / `auth.passwordFilePath` | ❌ | HANDLED-DIFFERENTLY | Operator should always mount secrets as files internally (more secure); this is an implementation choice, not a user knob to expose. | LOW |
| `auth.passwordRotation.{enabled,interval,resources}` (watcher sidecar) | ⚠️ ACL passwords rotate live via `ACL SETUSER` multi-password (ADR-008); but **default-user** rotation is unaddressed | HANDLED-DIFFERENTLY (ACL users) / SHOULD-ADD (default user) | The chart's poll-sidecar is replaced by the operator applying Secret changes live. But the *default* user's `requirepass` rotation must be wired once `spec.auth.passwordSecret` exists (support `keys[]` for multi-password). Fold into item #1/#2; no separate sidecar field. | HIGH |

> **Net auth finding:** the operator models ACL users richly (better than the
> chart's `acl.users`) but **completely omits the default-user / `requirepass`
> surface** the chart leads with. This is the single biggest security-relevant
> gap. See proposed `AuthSpec` in §3.

### 2.4 ACL (`acl.*`) — **SECURITY**

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `acl.enabled` | ⚠️ implicit (presence of `spec.users[]`) | COVERED | The operator enables ACL whenever `users[]` is non-empty. No explicit toggle needed. | — |
| `acl.users{}` (permissions, password, existingPasswordSecret) | ✅ `spec.users[]` (structured `commands`/`keys`/`channels`/`permissions`, `passwordSecret.keys[]`) | COVERED (richer) | CRD is *more* expressive than the chart's free-form `permissions` string. | — |
| `acl.replicationUser` (masteruser) | ❌ | CONSIDER | Add `spec.auth.replicationUser string` (name of an ACL user used for `masteruser`/`masterauth`). The operator may instead use `_operator`; if so, document as HANDLED-DIFFERENTLY. Decide during replication-mode impl. | MEDIUM |
| `acl.existingSecret` (BYO `users.acl`) | ❌ (operator renders `users.acl` itself) | HANDLED-DIFFERENTLY | The operator owns ACL-file rendering (ADR-008, `internal-<cluster>-acl`). A BYO raw ACL file bypasses that and risks `_operator` lockout; intentionally not exposed. | — |

### 2.5 TLS (`tls.*`) — **SECURITY**

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `tls.enabled` | ✅ (presence of `spec.tls`) | COVERED | — | — |
| `tls.existingSecret` (+ `certKey`/`keyKey`/`caKey`) | ⚠️ `spec.tls.secretName` (fixed keys `ca.crt`/`tls.crt`/`tls.key`) | CONSIDER | Add optional `spec.tls.secretKeys {ca,cert,key}` for non-standard key names. Minor; most users use standard keys. | LOW-MEDIUM |
| `tls.certManager.{issuerRef,duration,renewBefore,additionalDnsNames}` | ⚠️ `spec.tls.certManager.issuerRef` only | SHOULD-ADD | Add `duration`, `renewBefore`, `dnsNames []string` to `CertManagerSpec`. Cert lifetime/SAN control is a real cert-manager need. | MEDIUM |
| `tls.replication` (TLS for replication traffic) | ⚠️ operator auto-sets `tls-replication=yes` when TLS on (ADR-009) | COVERED | Always-on under TLS; matches hardened default. No knob needed. | — |
| **`tls.authClients`** (`yes`/`no`/`optional` — mTLS) | ❌ | **SHOULD-ADD** | Add `spec.tls.authClients enum{required,optional,disabled}` → `tls-auth-clients`. mTLS enforcement is a security control with no CRD equivalent. | **HIGH** |
| **`tls.disablePlaintext`** (`port 0`) | ⚠️ operator forces `port=0` whenever TLS on (ADR-009) | HANDLED-DIFFERENTLY / CONSIDER | Operator is *stricter* (always disables plaintext under TLS). Chart allows coexistence. Consider `spec.tls.allowPlaintextPort bool` if a migration use-case needs both ports. | MEDIUM |
| **`tls.dhParamsSecret`** | ❌ | **SHOULD-ADD** | Add `spec.tls.dhParamsSecret string` → `tls-dh-params-file`. Security knob, trivial to wire. | MEDIUM |
| **`tls.ciphers` / `tls.ciphersuites`** | ❌ | **SHOULD-ADD** | Add `spec.tls.ciphers` / `spec.tls.cipherSuites` → `tls-ciphers` / `tls-ciphersuites`. FIPS/compliance need. | MEDIUM |
| `tls.port` (6380) / `tls.certMountPath` | ❌ | HANDLED-DIFFERENTLY | Operator fixes the TLS port and mount path (ADR-009 renders `tls-port=6379`, mount `/etc/valkey/tls`). Not user knobs. | — |

### 2.6 Engine config (`config.*`)

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `config.maxmemory` / `maxmemoryPolicy` | ✅ `spec.config["maxmemory"/"maxmemory-policy"]` (live-settable) | COVERED | — | — |
| `config.bind` / `logLevel` / `disklessSync` | ✅ `spec.config[...]` (free-form map) | COVERED | Pass-through `config` map covers arbitrary non-managed keys. | — |
| `config.minReplicasToWrite` / `minReplicasMaxLag` | ✅ via `spec.config["min-replicas-to-write"/"min-replicas-max-lag"]` | COVERED | — | — |
| `config.extraFlags` (VALKEY_EXTRA_FLAGS) | ❌ | CONSIDER | Rarely needed given the `config` map; could add `spec.config` already covers most. Leave to `config`/`containers`. | LOW |
| `config.customConfig` (append raw valkey.conf) | ⚠️ `spec.config` map only (no raw blob) | CONSIDER | Add `spec.configBlob string` (raw appended valkey.conf) for directives that don't fit `key:value` (e.g. multiple `save` lines, `rename-command`). Operator-managed keys still win. | MEDIUM |
| `disableCommandsStandalone/Cluster/Sentinel` (per-mode) | ❌ | HANDLED-DIFFERENTLY | Subsumed by the single `spec.disableCommands` proposal (§2.13); per-mode override is a chart artifact (one mode per CR). | — |

### 2.7 Persistence (`persistence.*`)

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `persistence.enabled` | ✅ (presence of `spec.persistence` + `workloadType`) | COVERED | — | — |
| `persistence.size` / `storageClass` | ✅ `spec.persistence.{size,storageClassName}` | COVERED | (size expand-only, SC immutable — ADR-006) | — |
| `persistence.accessModes` | ❌ (operator hardcodes RWO) | SHOULD-ADD | Add `spec.persistence.accessModes []corev1.PersistentVolumeAccessMode` (default `[ReadWriteOnce]`). | MEDIUM |
| `persistence.keepOnUninstall` | ✅ `spec.persistence.reclaimPolicy: Retain` (default) | COVERED | — | — |
| `persistence.annotations` | ❌ | CONSIDER | Add `spec.persistence.annotations map[string]string` (PVC annotations — e.g. backup tooling tags). | LOW-MEDIUM |
| `persistence.subPath` / `persistence.hostPath` | ❌ | HANDLED-DIFFERENTLY | subPath/hostPath are niche; operator owns the data-dir layout. Not worth porting. | LOW |
| **`persistentVolumeClaimRetentionPolicy.{whenDeleted,whenScaled}`** | ⚠️ `reclaimPolicy` covers node-delete; **no `whenScaled`** | SHOULD-ADD | Extend `PersistenceSpec` with `whenScaled enum{Retain,Delete}` (the operator scales by adding/removing ValkeyNodes; orphaned PVCs on scale-in need a policy). | MEDIUM |

### 2.8 Health probes (`livenessProbe`/`readinessProbe`/`startupProbe`)

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `*.enabled`, `initialDelaySeconds`, `periodSeconds`, `timeoutSeconds`, `failureThreshold`, `successThreshold` | ❌ (operator-managed; `setProbeDefaults()` is a no-op placeholder) | CONSIDER | Add `spec.probes {liveness,readiness,startup *ProbeOverrides}` exposing the standard timing fields (operator owns the probe *command*). The design (03 §5) already reserves this slot. | MEDIUM |

### 2.9 Resources

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `resources.{limits,requests}` | ✅ `spec.resources` | COVERED | — | — |
| `initResources` (per init-container) | ❌ | CONSIDER | Operator's init containers (TLS setup, etc.) are operator-defined; expose `spec.initResources` if needed for constrained clusters. | LOW |
| **`resourcePreset`** (`nano`…`xlarge`) | ❌ | SHOULD-ADD (low effort) | Add `spec.resourcePreset enum{none,nano,micro,small,medium,large,xlarge}`; operator expands to requests/limits unless `spec.resources` set. Pure convenience, but a documented chart feature with fixed values (nano=100m/128Mi … xlarge=4/4Gi). | MEDIUM |

### 2.10 ServiceAccount — **SECURITY**

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `serviceAccount.create` / `name` / `annotations` | ❌ (no data-pod SA field) | **SHOULD-ADD** | Add `spec.serviceAccountName string` (data pods). Operator creates a default SA; allow BYO. | **HIGH** |
| `serviceAccount.automountServiceAccountToken` (default `false`) | ❌ | **SHOULD-ADD** | Add `spec.automountServiceAccountToken *bool` (default false — hardened). Security default the chart already ships. | **HIGH** |

### 2.11 PodDisruptionBudget (`pdb.*`)

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `pdb.enabled` | ✅ `spec.podDisruptionBudget: Managed\|Disabled` | COVERED | — | — |
| `pdb.minAvailable` / `maxUnavailable` | ⚠️ operator sizes the PDB to a per-shard quorum (03 §2.10) | HANDLED-DIFFERENTLY | The operator computes quorum-correct PDB sizing; a raw min/max would let users break quorum. Keep operator-managed. Consider an advanced override later. | LOW |

### 2.12 Service (`service.*`) + External Access (`externalAccess.*`)

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `service.type/port/annotations/clusterIP/loadBalancerClass/appProtocol` | ❌ (operator owns the headless Service; only `status.host`) | SHOULD-ADD | Add `spec.service {type, annotations, loadBalancerClass, ...}` for the client Service. At minimum annotations + type. | MEDIUM |
| **`externalAccess.enabled` + `externalAccess.service.*`** (LoadBalancer/NodePort, sourceRanges, externalTrafficPolicy) | ❌ | **SHOULD-ADD** | Add `spec.expose {enabled, type, annotations, loadBalancerSourceRanges, externalTrafficPolicy}` (PXC/PSMDB call this `spec.<component>.expose`). | **HIGH** |
| **`externalAccess.cluster.*`** (per-pod nodePorts/LB IPs + `cluster-announce-ip`) | ❌ | SHOULD-ADD (cluster mode) | Per-pod external access requires per-ValkeyNode Services + `cluster-announce-ip` discovery. Model as `spec.expose.perPod bool` (+ optional per-ordinal overrides); the operator does the announce-IP init that the chart's init container did. | HIGH |
| `externalAccess.standalone.{nodePort,tlsNodePort,loadBalancerIP}` | ❌ | CONSIDER | Covered by the general `spec.expose` for non-cluster modes; explicit nodePort pinning is a CONSIDER. | MEDIUM |

### 2.13 Disable commands (`disableCommands*`) — **SECURITY-adjacent**

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `disableCommands` (default `[FLUSHDB,FLUSHALL]`) | ❌ (cannot be set via `config` map — multi-valued `rename-command` + base-config-wins) | **SHOULD-ADD** | Add `spec.disableCommands []string`; operator renders `rename-command <CMD> ""` per entry. Default to `[FLUSHDB,FLUSHALL]` to match the chart's safe default. | **MEDIUM-HIGH** |

### 2.14 Network Policy (`networkPolicy.*`)

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `networkPolicy.{enabled,allowExternal,extraIngress,extraEgress}` | ❌ in CRD (ADR-009 says operator *generates* NetworkPolicy) | SHOULD-ADD | Add `spec.networkPolicy {enabled *bool, extraIngress, extraEgress}`. ADR-009 commits to NetworkPolicy generation but the CRD exposes no toggle/customization yet. | MEDIUM |

### 2.15 Init containers (`sysctlInit.*`, `volumePermissions.*`)

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `sysctlInit.{enabled,somaxconn,disableTHP,resources}` | ❌ | CONSIDER | Add `spec.sysctls map[string]string` (or a small `spec.sysctlInit` block) for `somaxconn`/THP tuning via a privileged init container. Perf knob; gated behind a flag. | MEDIUM |
| `volumePermissions.{enabled,resources}` | ❌ | HANDLED-DIFFERENTLY | `securityContext.fsGroup` (proposed §2.18) normally fixes PVC ownership; a root chown init container is a fallback for storage classes that ignore fsGroup. CONSIDER only if such a backend is targeted. | LOW |

### 2.16 Metrics / observability (`metrics.*`)

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `metrics.enabled` / `image` / `resources` | ✅ `spec.exporter.{enabled,image,resources}` | COVERED | — | — |
| `metrics.port` | ❌ | CONSIDER | Add `spec.exporter.port` (default 9121). Minor. | LOW |
| `metrics.{command,args,extraEnvs,extraVolumeMounts,extraSecrets,securityContext}` | ⚠️ partially via `spec.containers[]` strategic-merge of `metrics-exporter` | HANDLED-DIFFERENTLY | The `containers[]` patch can override the exporter container (env, args, securityContext). Document this path. | LOW |
| **`metrics.serviceMonitor.*`** (interval, labels, relabelings, scrapeTimeout, sampleLimit, honorLabels, …) | ❌ | SHOULD-ADD | Add `spec.exporter.serviceMonitor {enabled, interval, labels, relabelings, metricRelabelings, ...}`. Prometheus-Operator integration is expected of a production operator. | MEDIUM |
| **`metrics.podMonitor.*`** | ❌ | CONSIDER | Add `spec.exporter.podMonitor` (alternative to ServiceMonitor). | MEDIUM |
| **`metrics.prometheusRule.*`** (alert rules) | ❌ | CONSIDER | Add `spec.exporter.prometheusRule {enabled, labels, rules}` to ship default alert rules (ValkeyDown, etc.). | MEDIUM |

### 2.17 Lifecycle / graceful failover / diagnostic mode

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `gracefulFailover.enabled` (preStop CLUSTER FAILOVER) | ✅ operator-driven graceful `CLUSTER FAILOVER` before rolling a primary (ADR-007) | HANDLED-DIFFERENTLY (better) | The operator does proactive failover natively; the chart's preStop hook is its workaround. Strictly superior — no gap. | — |
| `lifecycle.{postStart,preStop}` (custom hooks) | ❌ | CONSIDER | Add `spec.lifecycle *corev1.Lifecycle` for advanced users. Low demand given operator-driven failover, but a recognized escape hatch. | LOW-MEDIUM |
| `diagnosticMode.{enabled,command,args}` (override entrypoint) | ❌ | CONSIDER | Add `spec.diagnosticMode {enabled, command, args}` to run pods with `sleep infinity` for debugging without losing the CR. Useful operational knob. | MEDIUM |

### 2.18 Security context (`securityContext`/`containerSecurityContext`) — **SECURITY**

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `securityContext` (runAsUser/Group, fsGroup, runAsNonRoot, seccompProfile) | ❌ | **SHOULD-ADD** | Add `spec.podSecurityContext *corev1.PodSecurityContext`. Operator sets a hardened default; allow override. | **HIGH** |
| `containerSecurityContext` (readOnlyRootFilesystem, allowPrivilegeEscalation, caps drop ALL) | ❌ | **SHOULD-ADD** | Add `spec.containerSecurityContext *corev1.SecurityContext`. Same rationale; needed for PSS/PSA compliance overrides. | **HIGH** |

### 2.19 Escape hatches (`extra*`, `env`, `extraEnvVars`)

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `extraContainers` / `extraInitContainers` | ⚠️ `spec.containers[]` (strategic-merge incl. append) covers sidecars; **no init-container append** | SHOULD-ADD (init) | `containers[]` covers extra sidecars. Add `spec.initContainers []corev1.Container` for extra init containers. | MEDIUM |
| `extraVolumes` / `extraVolumeMounts` | ❌ | SHOULD-ADD | Add `spec.volumes []corev1.Volume` + per-container mounts (or `spec.sidecarVolumes`). Needed to back `extraContainers`/secrets. | MEDIUM |
| `extraValkeySecrets` / `extraValkeyConfigs` (mount Secrets/ConfigMaps) | ❌ | CONSIDER | Convenience over `volumes`+`volumeMounts`; can be expressed once `spec.volumes` exists. CONSIDER sugar fields. | LOW |
| `env` (simple k/v) | ❌ | SHOULD-ADD | Add `spec.env map[string]string` (simple) — see top-10 #6. | MEDIUM-HIGH |
| `extraEnvVars` (full `corev1.EnvVar`) | ❌ | SHOULD-ADD | Add `spec.extraEnvVars []corev1.EnvVar` (valueFrom etc.). Pairs with `spec.env`. | MEDIUM-HIGH |

### 2.20 Scheduling extras (`priorityClassName`, `runtimeClassName`, `topologySpreadConstraints`, `podAntiAffinityPreset`, `dns*`, grace period, statefulset/pod metadata)

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `nodeSelector` / `tolerations` / `affinity` | ✅ `spec.{nodeSelector,tolerations,affinity}` | COVERED | — | — |
| `topologySpreadConstraints` | ✅ `spec.topologySpreadConstraints` (operator augments with shard selectors) | COVERED | — | — |
| `podAntiAffinityPreset.{type,topologyKey}` | ⚠️ only raw `affinity` | CONSIDER | Add `spec.podAntiAffinityPreset {type: soft\|hard, topologyKey}` sugar (PSMDB has `antiAffinityTopologyKey`). Lowers the bar vs hand-writing affinity. | MEDIUM |
| **`priorityClassName`** | ❌ | **SHOULD-ADD** | Add `spec.priorityClassName string`. Standard DB-pod knob (top-10 #8). | MEDIUM |
| **`runtimeClassName`** | ❌ | SHOULD-ADD | Add `spec.runtimeClassName *string` (gVisor/Kata). | MEDIUM |
| `terminationGracePeriodSeconds` (30) | ❌ | SHOULD-ADD | Add `spec.terminationGracePeriodSeconds *int64`. Data pods need tunable drain time. | MEDIUM |
| `dnsPolicy` / `dnsConfig` | ❌ | CONSIDER | Add `spec.dnsPolicy` / `spec.dnsConfig *corev1.PodDNSConfig`. Niche but standard. | LOW-MEDIUM |
| `podAnnotations` / `podLabels` | ❌ | SHOULD-ADD | Add `spec.annotations` / `spec.labels` (pod-level) — commonly required for mesh/scrape opt-in. Overlaps `commonLabels` (§2.1). | MEDIUM |
| `statefulset.{updateStrategy,podManagementPolicy,annotations,labels}` | ❌ | HANDLED-DIFFERENTLY | The operator owns rolling/update ordering (ADR-006/007); `updateStrategy`/`podManagementPolicy` would conflict with operator-driven rolls. Workload metadata could be CONSIDER, the strategy fields are BY-DESIGN. | LOW |

### 2.21 Autoscaling (`autoscaling.hpa`, `autoscaling.vpa`)

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `autoscaling.hpa.*` (HPA on standalone) | ❌ | HANDLED-DIFFERENTLY / BY-DESIGN | **Analyzed:** an HPA scaling the managed workloads directly conflicts with the operator, which *owns* `spec.shards`/`spec.replicas` and reconciles ValkeyNode count one-at-a-time with failover ordering (ADR-001/006/007). A user HPA editing replica counts would fight reconcile. The chart only allows HPA in *standalone* mode (no sharding) for this exact reason. **Do not expose pod-level HPA on cluster/replication.** If autoscaling is wanted, the correct model is the operator reacting to metrics and adjusting `shards`/`replicas` itself — a future feature, not a chart-parity gap. Mark BY-DESIGN with this rationale. | — |
| `autoscaling.vpa.*` (VPA) | ❌ | CONSIDER | VPA adjusts *resources*, not replica count, so it does **not** conflict with operator-owned topology. Could add `spec.vpa {enabled,updateMode,...}` or simply document that users may apply a VPA targeting the managed StatefulSets. Lower-risk than HPA. | MEDIUM |

### 2.22 Backup (`backup.*`)

| Chart value(s) | Operator/CRD status | Classification | Recommendation | Priority |
|----------------|---------------------|----------------|----------------|----------|
| `backup.enabled` / `schedule` | ✅ `spec.backup.schedule[]` (cron, robfig) | COVERED | — | — |
| `backup.retention` | ✅ `spec.backup.schedule[].keep` (+ Backup CR `retention.{keep,keepAge}`) | COVERED (richer) | — | — |
| `backup.concurrencyPolicy` | ⚠️ operator uses Lease-based serialization (ADR-004) | HANDLED-DIFFERENTLY | Operator serializes backups via Lease; `concurrencyPolicy: Forbid` semantics are the default behavior. No knob needed. | — |
| `backup.storage.*` (single PVC dest) | ✅ `spec.backup.storages{}` (s3/gcs/azure/filesystem) | COVERED (richer) | The operator targets object storage; the chart's single backup PVC ≈ `filesystem` (test-only). Object storage is strictly better. | — |
| `backup.sourceOrdinal` (which pod) | ⚠️ Backup CR `containerOptions.preferReplica` | HANDLED-DIFFERENTLY | Operator snapshots per-shard primaries (or prefers replica); pod-ordinal pinning is a chart artifact of its single-pod model. No gap. | — |
| `backup.{successfulJobsHistoryLimit,failedJobsHistoryLimit}` | ❌ | CONSIDER | Add `spec.backup.schedule[].{successfulJobsHistoryLimit,failedJobsHistoryLimit}` (or rely on finalizer GC). Minor. | LOW |
| `backup.resources` | ⚠️ Backup-tool resources not on cluster spec | CONSIDER | Add `spec.backup.resources corev1.ResourceRequirements` for the backup Job container. | LOW-MEDIUM |

---

## 3. Proposed `PerconaValkeyCluster` spec additions

Concrete new fields/sub-structs grouped by priority. Field names are Go-ish;
all secret material is **Secret-ref only** (never inline — ADR-008). Everything
added to `PerconaValkeyClusterSpec` that affects pods is propagated to
`ValkeyNodeSpec` (the parent→node contract, 03 §6).

### 3.1 CRITICAL / HIGH (security + core configurability)

```go
// --- Authentication of the default user (the chart's primary auth knob) ---
// New top-level field on PerconaValkeyClusterSpec.
type AuthSpec struct {
    // Enabled toggles default-user password auth (requirepass). Default true.
    Enabled *bool `json:"enabled,omitempty"`
    // PasswordSecret refs the Secret holding the default user's password(s).
    // keys[] enables multi-password rotation (live ACL SETUSER, no pod roll).
    PasswordSecret UserPasswordSecret `json:"passwordSecret,omitempty"`
    // ReplicationUser names an ACL user used for masteruser/masterauth
    // (replication mode). Empty => use the default/_operator user.
    ReplicationUser string `json:"replicationUser,omitempty"`
}
Auth AuthSpec `json:"auth,omitempty"`           // PerconaValkeyClusterSpec

// --- TLS hardening (extend the existing TLSConfig in shared_types.go) ---
// AuthClients controls mTLS: required|optional|disabled -> tls-auth-clients.
AuthClients   string `json:"authClients,omitempty"`   // TLSConfig
DHParamsSecret string `json:"dhParamsSecret,omitempty"` // -> tls-dh-params-file
Ciphers       string `json:"ciphers,omitempty"`        // -> tls-ciphers
CipherSuites  string `json:"cipherSuites,omitempty"`   // -> tls-ciphersuites
AllowPlaintextPort *bool `json:"allowPlaintextPort,omitempty"` // keep port open under TLS
// On CertManagerSpec: cert lifetime + extra SANs.
Duration    string   `json:"duration,omitempty"`    // CertManagerSpec
RenewBefore string   `json:"renewBefore,omitempty"`  // CertManagerSpec
DNSNames    []string `json:"dnsNames,omitempty"`     // CertManagerSpec

// --- Pod identity / security context (hardening parity) ---
ServiceAccountName            string                     `json:"serviceAccountName,omitempty"`
AutomountServiceAccountToken  *bool                      `json:"automountServiceAccountToken,omitempty"` // default false
PodSecurityContext            *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
ContainerSecurityContext      *corev1.SecurityContext    `json:"containerSecurityContext,omitempty"`

// --- External access / Service exposure ---
type ExposeSpec struct {
    Enabled                  bool                            `json:"enabled,omitempty"`
    Type                     corev1.ServiceType              `json:"type,omitempty"` // ClusterIP|NodePort|LoadBalancer
    Annotations              map[string]string               `json:"annotations,omitempty"`
    LoadBalancerClass        *string                         `json:"loadBalancerClass,omitempty"`
    LoadBalancerSourceRanges []string                        `json:"loadBalancerSourceRanges,omitempty"`
    ExternalTrafficPolicy    corev1.ServiceExternalTrafficPolicy `json:"externalTrafficPolicy,omitempty"`
    // PerPod creates a Service + cluster-announce-ip per ValkeyNode (cluster mode).
    PerPod                   bool                            `json:"perPod,omitempty"`
}
Expose ExposeSpec `json:"expose,omitempty"`        // PerconaValkeyClusterSpec
```

### 3.2 MEDIUM-HIGH / MEDIUM (configurability parity)

```go
// Dangerous-command disabling (rename-command <CMD> "").
DisableCommands []string `json:"disableCommands,omitempty"` // default [FLUSHDB,FLUSHALL]

// Extra env on the server container.
Env          map[string]string `json:"env,omitempty"`
ExtraEnvVars []corev1.EnvVar   `json:"extraEnvVars,omitempty"`

// Scheduling extras.
PriorityClassName             string                 `json:"priorityClassName,omitempty"`
RuntimeClassName              *string                `json:"runtimeClassName,omitempty"`
TerminationGracePeriodSeconds *int64                 `json:"terminationGracePeriodSeconds,omitempty"`
ImagePullPolicy               corev1.PullPolicy      `json:"imagePullPolicy,omitempty"`
PodAntiAffinityPreset         *AntiAffinityPreset    `json:"podAntiAffinityPreset,omitempty"` // {type: soft|hard, topologyKey}

// Pod / object metadata propagation.
Annotations map[string]string `json:"annotations,omitempty"` // pod annotations
Labels      map[string]string `json:"labels,omitempty"`      // pod labels (+ commonLabels equiv)

// Resource preset convenience (overridden by spec.resources).
ResourcePreset string `json:"resourcePreset,omitempty"` // none|nano|micro|small|medium|large|xlarge

// Persistence extras (extend PersistenceSpec).
AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"` // default [RWO]
WhenScaled  ReclaimPolicy                       `json:"whenScaled,omitempty"`  // PVC policy on scale-in
Annotations map[string]string                   `json:"annotations,omitempty"` // PVC annotations

// Init containers + volumes (escape hatches).
InitContainers []corev1.Container   `json:"initContainers,omitempty"`
Volumes        []corev1.Volume      `json:"volumes,omitempty"`
VolumeMounts   []corev1.VolumeMount `json:"volumeMounts,omitempty"`

// Observability: Prometheus-Operator CR generation (extend ExporterSpec).
type ServiceMonitorSpec struct {
    Enabled          bool              `json:"enabled,omitempty"`
    Interval         string            `json:"interval,omitempty"`
    ScrapeTimeout    string            `json:"scrapeTimeout,omitempty"`
    Labels           map[string]string `json:"labels,omitempty"`
    Relabelings      []interface{}     `json:"relabelings,omitempty"`
    MetricRelabelings []interface{}    `json:"metricRelabelings,omitempty"`
    HonorLabels      bool              `json:"honorLabels,omitempty"`
}
ServiceMonitor *ServiceMonitorSpec `json:"serviceMonitor,omitempty"` // ExporterSpec
Port           int32               `json:"port,omitempty"`           // ExporterSpec, default 9121

// NetworkPolicy generation toggle/customization.
type NetworkPolicySpec struct {
    Enabled      *bool         `json:"enabled,omitempty"`
    ExtraIngress []interface{} `json:"extraIngress,omitempty"`
    ExtraEgress  []interface{} `json:"extraEgress,omitempty"`
}
NetworkPolicy NetworkPolicySpec `json:"networkPolicy,omitempty"` // PerconaValkeyClusterSpec

// Cluster engine tuning that base-config currently swallows.
ClusterNodeTimeout *int32 `json:"clusterNodeTimeout,omitempty"` // -> cluster-node-timeout
ConfigBlob         string `json:"configBlob,omitempty"`         // raw appended valkey.conf
```

### 3.3 MEDIUM / LOW (operational niceties)

```go
// Probe timing overrides (operator owns the probe command; design slot reserved 03 §5).
type ProbeOverrides struct {
    InitialDelaySeconds *int32 `json:"initialDelaySeconds,omitempty"`
    PeriodSeconds       *int32 `json:"periodSeconds,omitempty"`
    TimeoutSeconds      *int32 `json:"timeoutSeconds,omitempty"`
    FailureThreshold    *int32 `json:"failureThreshold,omitempty"`
    SuccessThreshold    *int32 `json:"successThreshold,omitempty"`
}
type ProbesSpec struct {
    Liveness  *ProbeOverrides `json:"liveness,omitempty"`
    Readiness *ProbeOverrides `json:"readiness,omitempty"`
    Startup   *ProbeOverrides `json:"startup,omitempty"`
}
Probes ProbesSpec `json:"probes,omitempty"`

// Operational/escape hatches.
Lifecycle      *corev1.Lifecycle `json:"lifecycle,omitempty"`
DiagnosticMode *struct {
    Enabled bool     `json:"enabled,omitempty"`
    Command []string `json:"command,omitempty"`
    Args    []string `json:"args,omitempty"`
} `json:"diagnosticMode,omitempty"`
DNSPolicy corev1.DNSPolicy    `json:"dnsPolicy,omitempty"`
DNSConfig *corev1.PodDNSConfig `json:"dnsConfig,omitempty"`
ClusterServiceDNSSuffix string `json:"clusterServiceDNSSuffix,omitempty"` // chart clusterDomain

// sysctl tuning init container.
type SysctlInitSpec struct {
    Enabled    bool                        `json:"enabled,omitempty"`
    Somaxconn  *int                        `json:"somaxconn,omitempty"`
    DisableTHP bool                        `json:"disableTHP,omitempty"`
    Resources  corev1.ResourceRequirements `json:"resources,omitempty"`
}
SysctlInit *SysctlInitSpec `json:"sysctlInit,omitempty"`

// Backup-job ergonomics.
// On BackupSpec: Resources for the backup Job container.
Resources corev1.ResourceRequirements `json:"resources,omitempty"` // BackupSpec
```

> **Sequencing recommendation.** Implement §3.1 first (security-critical:
> default-user auth, TLS mTLS/ciphers, SA/token, security contexts, external
> access). §3.2 next (escape hatches + observability). §3.3 as an
> ergonomics/polish milestone.

---

## 4. Out of scope by design (NOT gaps)

These chart capabilities are intentionally **not** mirrored in the CRD. Each has
an ADR or architectural rationale; listing them prevents future "missing field"
churn.

| Chart capability | Rationale | Ref |
|------------------|-----------|-----|
| **`sentinel.*` entire block** (`sentinelReplicas`, `quorum`, `downAfterMilliseconds`, `failoverTimeout`, `parallelSyncs`, Sentinel StatefulSet, masterSet, sentinel resources/persistence/scheduling) | The operator uses **operator-driven failover, NO Sentinel**, in all modes. `replication` mode replaces Sentinel's master/replica HA with controller-driven, offset-aware promotion. There is no Sentinel process to configure. | **ADR-007** |
| **`mode: sentinel`** | Same — replaced by `mode: replication`. | ADR-007 |
| **Cluster-init / cluster-scale / cluster-precheck Helm Jobs** (`cluster.precheckBeforeScaleDown`, init/scale job images `image.jobs.*`) | The operator's reconcile performs MEET / REPLICATE / slot-assign / rebalance / safe scale-down. These Helm Jobs are the chart's substitute for a controller. No user knob. | ADR-001, 04-control-plane |
| **`test-connection` Job / Helm test hooks** | Replaced by operator status conditions + readiness probes. | ADR-001 |
| **Backup CronJob (`backup.*` as a Helm CronJob)** | The operator runs cron *inside reconcile* (robfig) and spawns backup Jobs; the *user-facing* schedule knobs ARE covered (`spec.backup.schedule[]`). Only the CronJob-as-resource is replaced. | ADR-004 |
| **`gracefulFailover.enabled` (preStop hook)** | Operator does proactive `CLUSTER FAILOVER` before rolling a primary natively — strictly better than a preStop hook. | ADR-007 |
| **`statefulset.updateStrategy` / `podManagementPolicy`** | The operator owns pod-roll *ordering* (replicas-before-primary, one-at-a-time); ceding it to a StatefulSet rolling strategy would break failover ordering. | ADR-006, ADR-007 |
| **`autoscaling.hpa.*` (HPA on managed workloads)** | An HPA editing replica counts fights the operator, which owns `shards`/`replicas` and reconciles ValkeyNode count with failover ordering. The chart itself restricts HPA to *standalone* for this reason. Topology autoscaling, if ever wanted, belongs in the operator reacting to metrics — not a user HPA. | ADR-001/006/007 (analysis in §2.21) |
| **`auth.usePasswordFiles` / `passwordFilePath`** | Implementation detail: the operator always mounts secrets as files internally (more secure than env). Not a user-facing choice. | ADR-008 |
| **`acl.existingSecret` (BYO raw `users.acl`)** | The operator owns ACL-file rendering to guarantee `_operator`/`_exporter` are present (lock-out guardrail). A raw BYO file would bypass that. | ADR-008 |
| **`image.variant: rpm\|hardened` (RPM/UBI9 vs DHI/distroless)** | The CRD takes a **full image reference** in `spec.image`; the user selects RPM vs hardened by *which tag they pin* (`percona/percona-valkey:9.0.0` vs `...:9.0.0-hardened`). The version service / docs `*recommended` pins surface the variant choice. A `variant` enum is a chart-templating convenience that would duplicate tag selection — **not a gap**, though docs MUST document the hardened tag and the hardened security-context implications. (If product wants a friendlier selector, a `spec.image` + documented tag convention suffices.) | 03 §2.2 |
| **`image.jobs.*`** (separate shell-tool image for Jobs) | The operator's sidecar/helper binaries (`cmd/valkey-backup`, `cmd/healthcheck`, `cmd/peer-list`) are baked/managed by the operator; there is no shell-tool Job image for users to override. | ADR-002, ADR-010 |
| **`global.imageRegistry`, `nameOverride`, `fullnameOverride`** | Helm-templating conveniences; the CR's `metadata.name` and full image refs replace them. | ADR-003 |

> **Sentinel carry-over note (explicitly requested).** Although Sentinel itself
> is out of scope, the *failover-tuning intent* behind some Sentinel knobs is
> real. The operator should expose equivalents where they map to its own
> failover loop — e.g. a future `spec.failover { failoverTimeoutSeconds,
> downAfterSeconds }` mapping to the controller's promotion poll/timeout budget
> (the ~10s `CLUSTER FAILOVER` poll in ADR-007). This is **not chart parity**
> (the chart's values configure the Sentinel process, not a controller), so it
> is tracked as a *CONSIDER* future operator-native knob, not a SHOULD-ADD gap.

---

## 5. Already covered (parity confirmed)

For completeness, the chart knobs that map cleanly (often more richly) to the CRD
today: `image.pullSecrets` → `imagePullSecrets`; `cluster.replicas`/`replicasPerPrimary`
→ `shards`+`replicas`; `standalone.useDeployment` → `workloadType=Deployment`;
`acl.users{}` → `users[]` (structured, richer); `tls.enabled`/`existingSecret`/`certManager.issuerRef`
→ `tls.secretName`/`certManager`; `config.maxmemory*`/`bind`/`logLevel`/`minReplicas*`
→ `config` map; `persistence.enabled`/`size`/`storageClass`/`keepOnUninstall`
→ `persistence` + `reclaimPolicy`; `resources` → `resources`; `pdb.enabled`
→ `podDisruptionBudget`; `metrics.enabled`/`image`/`resources` → `exporter`;
`backup.schedule`/`retention`/`storage` → `backup.storages`+`schedule` (object-storage,
richer); `nodeSelector`/`tolerations`/`affinity`/`topologySpreadConstraints` → same;
`upgradeOptions` (Percona addition, no chart equivalent). The operator also adds
`crVersion` gating, `pause`, first-class Backup/Restore CRs, and slot-coverage-aware
restore — capabilities the chart lacks entirely.

---

## 6. Summary of the actionable backlog

- **CRITICAL (2):** default-user `spec.auth.{enabled,passwordSecret}`.
- **HIGH (6):** TLS `authClients`/dhParams/ciphers; `serviceAccountName` +
  `automountServiceAccountToken`; pod/container `securityContext`; external
  access / Service exposure (`spec.expose`, incl. per-pod cluster mode);
  default-user password rotation wiring.
- **MEDIUM-HIGH (3):** `spec.env`/`extraEnvVars`; `spec.disableCommands`;
  cert-manager duration/renewBefore/SANs.
- **MEDIUM (≈12):** `priorityClassName`, `runtimeClassName`,
  `terminationGracePeriodSeconds`, `imagePullPolicy`, `resourcePreset`,
  persistence `accessModes`/`whenScaled`/annotations, `clusterNodeTimeout`/`configBlob`,
  ServiceMonitor/PodMonitor/PrometheusRule, NetworkPolicy toggle, `initContainers`/`volumes`,
  pod `annotations`/`labels`/`commonLabels`, `clusterDomain`, `podAntiAffinityPreset`,
  diagnosticMode, probes overrides, VPA documentation.
- **Out of scope by design (11):** Sentinel block + mode, Helm orchestration
  Jobs, HPA on managed workloads, password-file impl detail, BYO raw ACL file,
  image `variant` selector, `image.jobs`, registry/name overrides — all ADR-backed.

The single highest-leverage milestone is **§3.1 (security + core
configurability)**: it closes every CRITICAL/HIGH gap and brings the operator to
hardening + auth parity with the chart while preserving the deliberate
architectural differences (operator-driven failover, controller orchestration,
object-storage backups).
