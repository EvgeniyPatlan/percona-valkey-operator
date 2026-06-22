# Performance & scale harness (`e2e-tests/perf/`)

Owner: OPS-8.5 (LEG B). Implements the performance/scale layer of the test pyramid
(docs/architecture/11-testing-qa.md §5). These tests are **NOT PR-gated** — they are too
noisy on shared runners. They run **nightly** (`perf-smoke`) and **at release**
(`perf-scale`) against a real cluster (Kind locally, GKE on Jenkins) with
production-shaped resource requests, and they persist diffable trend artifacts so
regressions surface across runs.

> **HONESTY:** like the kuttl e2e/chaos suites, this harness is AUTHORED for
> CI/Jenkins/laptop-Kind. It is **not run here** — there is no live cluster and no built
> operator image in the authoring environment. `bash -n` / YAML-parse validation only.

## What it measures (arch §5)

| Metric | How | Profile |
|--------|-----|---------|
| **Throughput / latency** | `valkey-benchmark` (cluster-mode aware via `--cluster`), p50/p99 GET/SET, ops/sec at a fixed payload size | both |
| **Exporter overhead** | run the benchmark twice — WITH and WITHOUT the exporter sidecar (`spec.exporter.enabled`) — and report the ops/sec & p99 delta | both |
| **Rebalance throughput** | wall-clock to rebalance N slots + number of reconciles during scale-out/in (the operator does **one `PlanRebalanceMove` per reconcile**, ~30s pacing — so the meaningful metric is wall-clock + reconcile count, NOT raw migrate bandwidth) | perf-scale |
| **Scale-out latency** | time from `shards: M → N` to `cluster_state:ok` with even slot distribution (~`16384/N` per primary, ±1) | perf-scale |
| **Rolling-update window** | time + write-availability during a full one-at-a-time rolling restart with proactive failover | perf-scale |

**Metrics source:** the exporter sidecar (`PodMonitor`/`ServiceMonitor`) plus
operator-emitted metrics (failover count per shard, rebalance moves, config-mismatch
gauge). See architecture/08-observability.md. Runs are persisted as CSV/JSON to
`$PERF_ARTIFACT_DIR` (object storage on CI) so nightly trends are diffable.

## Layout

```
e2e-tests/perf/
  README.md                 # this file — methodology + thresholds
  lib-perf.sh               # shared config + helpers (sourced by run-perf.sh)
  run-perf.sh               # the driver: provision → benchmark → scrape → trend → gate
  benchmark-job.yaml        # valkey-benchmark Kubernetes Job template (envsubst)
  profiles/
    perf-smoke.env          # nightly: 1×3-shard, 60s benchmark, trend only
    perf-scale.env          # release: 6→12 shards, large keyspace, full metrics
```

## Running

```bash
# nightly trend (small, fast):
PROFILE=perf-smoke IMAGE=perconalab/valkey-operator:main ./run-perf.sh

# release scale profile (large, slow):
PROFILE=perf-scale IMAGE=perconalab/valkey-operator:main ./run-perf.sh

# compare a run against a stored baseline and enforce the p99 regression gate:
PROFILE=perf-smoke BASELINE=artifacts/perf-smoke-baseline.json ./run-perf.sh
```

The driver assumes an already-provisioned cluster + the operator image is loadable
(Kind: `kind load`; GKE: pushed). It deploys a perf cluster, waits Ready, runs the
`valkey-benchmark` Job, parses its output, writes a trend record, and — if `BASELINE`
is set — fails when p99 regresses beyond the threshold.

## Methodology details

- **Cluster-mode aware.** `valkey-benchmark --cluster` follows `-MOVED`/`-ASK` so the
  load spreads across all shards rather than hammering one primary. Connects through the
  headless Service; the benchmark discovers the topology from `CLUSTER NODES`.
- **Fixed payload + key space.** Payload size and `--keyspacelen` are pinned per profile
  so runs are comparable over time. Randomized keys (`-r`) spread across CRC16 keyslots.
- **Warmup discarded.** A short warmup load precedes the measured window so JIT/page-cache
  effects don't skew p99.
- **Eviction behaviour (arch §5).** A dedicated phase sets `maxmemory` +
  `maxmemory-policy: allkeys-lru` via the CR (live-settable → asserts the policy takes
  effect WITHOUT a pod roll), then drives the keyspace past `maxmemory` and verifies
  eviction (`evicted_keys` from `INFO stats` climbs; key count plateaus).
- **One move per reconcile.** Rebalance throughput is reported as wall-clock + reconcile
  count, never raw bandwidth — the operator deliberately paces slot moves (arch §5,
  data-plane §179) to give cluster-aware clients time to absorb each `-MOVED`.

## Target thresholds (regression gates)

These are **regression** gates (relative to a stored baseline), not absolute SLOs —
absolute numbers depend on node shape and are recorded, not asserted.

| Gate | Default | Override | Source |
|------|---------|----------|--------|
| **p99 GET/SET regression** | fail if p99 regresses **> `PERF_P99_REGRESS_PCT` (default 20)%** vs baseline | `PERF_P99_REGRESS_PCT` | arch §5 ("fail if p99 regresses > N%") |
| **ops/sec regression** | warn if ops/sec drops **> `PERF_OPS_REGRESS_PCT` (default 15)%** | `PERF_OPS_REGRESS_PCT` | derived |
| **scale-out latency** | record only (no hard gate until a baseline window exists) | — | arch §5 + OPEN QUESTION #1 |
| **exporter overhead** | record the WITH/WITHOUT delta; warn if exporter costs **> `PERF_EXPORTER_OVERHEAD_PCT` (default 5)%** ops/sec | `PERF_EXPORTER_OVERHEAD_PCT` | arch §5 |

> **OPEN QUESTION #1 (impl §13):** the architecture says "fail if p99 regresses > N%" but
> does NOT fix `N`. This harness defaults `N=20` for p99 and ships the trend artifact
> first; the team pins the real threshold after a baseline window. The default is a
> deliberately loose starting point, NOT a charter-blessed SLO — do not treat a pass at
> the default as a performance guarantee.
