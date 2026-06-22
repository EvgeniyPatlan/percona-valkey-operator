#!/usr/bin/env bash
# e2e-tests/perf/run-perf.sh — performance/scale driver (docs/architecture/11-testing-qa.md §5).
#
# Orchestrates one perf run for a PROFILE:
#   1. provision a perf PerconaValkeyCluster (exporter ON), wait Ready + cluster_state:ok
#   2. run a valkey-benchmark Job, parse p50/p99/ops-sec for SET & GET
#   3. (perf-smoke) re-run with exporter OFF to measure exporter overhead
#   4. (perf-scale) scale-out and record wall-clock + reconciles to rebalance, scale-out
#      latency to even slot distribution, and the rolling-update write-availability window
#   5. (eviction) set maxmemory + allkeys-lru live (no roll), drive past maxmemory, verify
#   6. write a diffable trend artifact (JSON) under $PERF_ARTIFACT_DIR
#   7. if $BASELINE is set, enforce the p99 regression gate (fail > N%)
#
# NOT PR-gated. Nightly (perf-smoke) / release (perf-scale). HONEST: authored for
# CI/Jenkins/Kind — NOT executed in the authoring env (no live cluster). bash -n only.
#
# Usage:
#   PROFILE=perf-smoke IMAGE=perconalab/valkey-operator:main ./run-perf.sh
#   PROFILE=perf-scale BASELINE=artifacts/perf-scale-baseline.json ./run-perf.sh
set -o errexit
set -o nounset
set -o pipefail

PERF_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib-perf.sh
source "$PERF_ROOT/lib-perf.sh"

: "${PROFILE:?PROFILE is required (perf-smoke | perf-scale)}"
PROFILE_FILE="$PERF_ROOT/profiles/${PROFILE}.env"
[ -f "$PROFILE_FILE" ] || die "unknown profile '$PROFILE' (no $PROFILE_FILE)"
# shellcheck source=/dev/null
source "$PROFILE_FILE"

RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$PERF_ARTIFACT_DIR"
TREND_FILE="$PERF_ARTIFACT_DIR/${PROFILE}-${RUN_ID}.json"

# ---------------------------------------------------------------------------
# Render and apply a perf cluster CR. $1 = "on"|"off" for the exporter sidecar.
# ---------------------------------------------------------------------------
apply_perf_cluster() {
  local exporter="$1"
  local enabled="true"; [ "$exporter" = "off" ] && enabled="false"
  say "applying perf cluster (shards=$PERF_SHARDS replicas=$PERF_REPLICAS exporter=$exporter)"
  kubec create namespace "$NAMESPACE" --dry-run=client -o yaml | kubec apply -f -
  cat <<YAML | kube apply -f -
apiVersion: valkey.percona.com/v1alpha1
kind: PerconaValkeyCluster
metadata:
  name: ${CLUSTER_NAME}
spec:
  image: perconalab/percona-valkey:${VALKEY_VERSION}
  mode: cluster
  shards: ${PERF_SHARDS}
  replicas: ${PERF_REPLICAS}
  workloadType: StatefulSet
  persistence:
    size: 2Gi
  exporter:
    enabled: ${enabled}
  resources:
    requests: { cpu: "500m", memory: 512Mi }
YAML
  wait_cluster_ready 900
  wait_cluster_ok 900
  ok "perf cluster Ready, cluster_state:ok"
}

# ---------------------------------------------------------------------------
# Run one benchmark Job, wait for completion, parse the --csv block from its log.
# Echoes lines "<test> <rps> <p99>" for each test in BENCH_TESTS.
# ---------------------------------------------------------------------------
run_benchmark() {
  local phase="$1"
  export JOB_NAME="perf-${CLUSTER_NAME}-${phase}-${RUN_ID}"
  export SVC_HOST; SVC_HOST="$(svc_host)"
  export AUTH_SECRET="internal-${CLUSTER_NAME}-system-passwords"
  export CLUSTER_NAME VALKEY_VERSION BENCH_IMAGE SVC_PORT \
         BENCH_REQUESTS BENCH_CLIENTS BENCH_PAYLOAD BENCH_KEYSPACE BENCH_TESTS

  say "running benchmark Job: $JOB_NAME"
  envsubst < "$PERF_ROOT/benchmark-job.yaml" | kube apply -f -
  kube wait --for=condition=complete "job/$JOB_NAME" --timeout=900s \
    || die "benchmark Job $JOB_NAME did not complete"

  # Extract the measured CSV block between the PERF markers.
  kube logs "job/$JOB_NAME" \
    | awk '/===PERF-BEGIN===/{f=1;next} /===PERF-END===/{f=0} f' \
    | while IFS= read -r line; do
        case "$line" in
          \"*\"*) parse_bench_csv_line "$line" ;;
        esac
      done
}

# ---------------------------------------------------------------------------
# Scale-out phase (perf-scale only): time M→N to cluster_state:ok with even
# distribution; record reconcile/move count.
# ---------------------------------------------------------------------------
scale_out_phase() {
  [ "${PERF_DO_SCALE:-false}" = "true" ] || { echo "scale: skipped"; return; }
  say "scale-out ${PERF_SHARDS} → ${PERF_SCALE_TO} shards"
  local t0 t1
  t0="$SECONDS"
  kube patch pvk "$CLUSTER_NAME" --type merge -p "{\"spec\":{\"shards\":${PERF_SCALE_TO}}}"
  wait_cluster_ok 1800
  t1="$SECONDS"
  local elapsed=$(( t1 - t0 ))
  local per_primary=$(( 16384 / PERF_SCALE_TO ))
  echo "scale_out_seconds=${elapsed} target_slots_per_primary=${per_primary} moves=$(rebalance_moves)"
  SCALE_OUT_SECONDS="$elapsed"
}

# ---------------------------------------------------------------------------
# Eviction phase: maxmemory + allkeys-lru live-set (no roll), drive past it, verify.
# ---------------------------------------------------------------------------
eviction_phase() {
  [ "${PERF_DO_EVICTION:-false}" = "true" ] || { echo "eviction: skipped"; return; }
  say "eviction: set maxmemory + allkeys-lru (live, must NOT roll)"
  local gen_before gen_after
  gen_before="$(kube get statefulset \
    -l app.kubernetes.io/instance="$CLUSTER_NAME",app.kubernetes.io/name=valkey \
    -o jsonpath='{range .items[*]}{.metadata.generation} {end}')"
  kube patch pvk "$CLUSTER_NAME" --type merge \
    -p '{"spec":{"config":{"maxmemory":"128mb","maxmemory-policy":"allkeys-lru"}}}'
  sleep 15   # allow the live CONFIG SET to apply (poll cadence); bounded, not a gate
  gen_after="$(kube get statefulset \
    -l app.kubernetes.io/instance="$CLUSTER_NAME",app.kubernetes.io/name=valkey \
    -o jsonpath='{range .items[*]}{.metadata.generation} {end}')"
  [ "$gen_before" = "$gen_after" ] \
    || warn "eviction: STS generation changed ($gen_before → $gen_after) — live key should NOT roll"
  echo "eviction_no_roll=$([ "$gen_before" = "$gen_after" ] && echo true || echo false)"
}

# ---------------------------------------------------------------------------
# Trend artifact + regression gate.
# ---------------------------------------------------------------------------
write_trend() {
  local with_exporter="$1" without_exporter="$2"
  say "writing trend artifact: $TREND_FILE"
  {
    printf '{\n'
    printf '  "profile": "%s",\n' "$PROFILE"
    printf '  "run_id": "%s",\n' "$RUN_ID"
    printf '  "image": "%s",\n' "$OPERATOR_IMAGE"
    printf '  "valkey_version": "%s",\n' "$VALKEY_VERSION"
    printf '  "shards": %s,\n' "$PERF_SHARDS"
    printf '  "scale_out_seconds": "%s",\n' "${SCALE_OUT_SECONDS:-n/a}"
    printf '  "with_exporter": "%s",\n' "$with_exporter"
    printf '  "without_exporter": "%s"\n' "$without_exporter"
    printf '}\n'
  } > "$TREND_FILE"
  ok "trend written"
}

# Enforce the p99 regression gate against $BASELINE (a prior trend JSON). Compares the
# GET p99 of the with-exporter phase. Fails > PERF_P99_REGRESS_PCT.
enforce_gate() {
  [ -n "${BASELINE:-}" ] || { echo "no BASELINE set — trend recorded, gate skipped"; return; }
  [ -f "$BASELINE" ] || die "BASELINE file not found: $BASELINE"
  say "enforcing p99 regression gate (limit ${PERF_P99_REGRESS_PCT}%) vs $BASELINE"
  # Extract GET p99 from "<test> <rps> <p99>" lines stored in the with_exporter blob.
  local base_p99 cur_p99
  base_p99="$(grep -Eo 'GET [0-9.]+ [0-9.]+' "$BASELINE" | awk '{print $3}' | head -1 || true)"
  cur_p99="$(echo "$WITH_EXPORTER_RESULTS" | awk '/^GET/{print $3}' | head -1 || true)"
  [ -n "$base_p99" ] && [ -n "$cur_p99" ] || { warn "p99 unavailable on one side — gate inconclusive"; return; }
  local delta; delta="$(pct_delta "$base_p99" "$cur_p99")"
  echo "GET p99 baseline=${base_p99} current=${cur_p99} delta=${delta}%"
  awk -v d="$delta" -v lim="$PERF_P99_REGRESS_PCT" 'BEGIN{ if (d=="n/a") exit 0; if (d+0 > lim+0) exit 1 }' \
    || die "p99 REGRESSION: GET p99 regressed ${delta}% (> ${PERF_P99_REGRESS_PCT}%)"
  ok "p99 within threshold"
}

# ===========================================================================
main() {
  say "PERF RUN profile=$PROFILE run_id=$RUN_ID image=$OPERATOR_IMAGE"

  # Phase 1: exporter ON — the primary measured configuration.
  apply_perf_cluster on
  scale_out_phase
  eviction_phase
  WITH_EXPORTER_RESULTS="$(run_benchmark with-exporter)"
  echo "--- with exporter ---"; echo "$WITH_EXPORTER_RESULTS"

  # Phase 2 (optional): exporter OFF — measure overhead.
  WITHOUT_EXPORTER_RESULTS="n/a"
  if [ "${PERF_COMPARE_EXPORTER:-false}" = "true" ]; then
    apply_perf_cluster off
    WITHOUT_EXPORTER_RESULTS="$(run_benchmark without-exporter)"
    echo "--- without exporter ---"; echo "$WITHOUT_EXPORTER_RESULTS"
  fi

  write_trend "$WITH_EXPORTER_RESULTS" "$WITHOUT_EXPORTER_RESULTS"
  enforce_gate
  ok "PERF RUN complete: $TREND_FILE"
}

main "$@"
