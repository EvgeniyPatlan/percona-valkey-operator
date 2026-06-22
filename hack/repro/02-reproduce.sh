#!/usr/bin/env bash
# Deterministic before/after demonstration of a root cause (WORKED REFERENCE — adapt per issue).
#
# Canonical technique (from K8SPS-732, arch §7.3): scale the operator to 0 so it cannot
# revert live state, isolate the SINGLE variable the fix changes, drive the failing
# condition, observe a definite signal, restore. End with a machine-greppable
# REPRODUCED:/VERDICT line so QA Level B can grep the outcome.
#
# This file ships a fully-worked SHAPE (b) reproduction — the config-hash spurious-roll
# invariant (arch §2.2 item 5, the operator's single highest-value assertion):
#
#   A LIVE-settable key (maxmemory / maxmemory-policy / maxclients) is EXCLUDED from the
#   config-roll hash, so changing only such a key must NOT roll any StatefulSet. The signal
#   is the sum of StatefulSet .metadata.generation across the cluster: a roll bumps it, a
#   live-only CONFIG SET must leave it unchanged.
#
#     BEFORE (bug): editing spec.config.maxmemory bumps the STS generation (spurious roll).
#     AFTER  (fix): the same edit leaves the STS generation unchanged (no roll).
#
# Why shape (b) is the default reference: it is fully DETERMINISTIC (no quorum-loss race,
# no special cache-mode CR), needs only the unpatched repo bundle to demonstrate the
# expected (fixed) behaviour, and exercises the highest-value invariant in the operator.
#
# To adapt to SHAPE (a) instead — a cache-mode (no-persistence) shard whose primary loses
# quorum, where an unpatched binary stays Degraded and the patched operator runs
# promoteOrphanedReplicas → CLUSTER FAILOVER TAKEOVER and converges to Ready (emitting
# ReplicasTakenOver) — point cr-cluster.yaml at a cache-mode shard, then replace the
# "drive the variable" block below with: resolve the primary via current_primary_pod,
# `k delete pod` it, and compare cluster_state() Degraded→Ready across the operator restart.
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

WINDOW="${WINDOW:-120}"                 # seconds to allow convergence per phase
LIVE_KEY="${LIVE_KEY:-maxmemory}"       # a live-settable key (excluded from the roll hash)
LIVE_VAL="${LIVE_VAL:-512mb}"           # the new value to set

[ "$(cluster_state)" = "Ready" ] || { warn "cluster is not 'Ready'; run ./01-setup.sh first"; exit 1; }

say "scale operator to 0 so it cannot reconcile while we change one variable by hand"
k scale "deploy/$OPERATOR_DEPLOY" --replicas=0 >/dev/null
# wait for the operator pod to actually be gone, so the live edit below is ours alone
for _ in $(seq 1 20); do
  [ "$(k get pod -l app.kubernetes.io/name=percona-valkey-operator --no-headers 2>/dev/null | grep -c .)" = "0" ] && break
  sleep 2
done

gen_before="$(sts_generation_sum)"
say "baseline StatefulSet generation sum = ${gen_before}"

# --- drive the SINGLE variable the fix changes (ADAPT PER ISSUE) --------------------
# Change ONLY a live-settable key. With the operator down this just edits the CR; the
# generation cannot move yet. We scale the operator back up and watch whether IT rolls.
say "editing spec.config.$LIVE_KEY=$LIVE_VAL (a LIVE-settable key — must NOT trigger a roll)"
k patch pvk "$CR_NAME" --type=merge \
  -p "{\"spec\":{\"config\":{\"$LIVE_KEY\":\"$LIVE_VAL\"}}}" >/dev/null
# -----------------------------------------------------------------------------------

say "AFTER: scale operator back; a CORRECT build applies the key live (CONFIG SET), no roll"
k scale "deploy/$OPERATOR_DEPLOY" --replicas=1 >/dev/null
k rollout status "deploy/$OPERATOR_DEPLOY" --timeout="${WINDOW}s" >/dev/null 2>&1 || true

# Give the operator a couple of reconciles to (correctly) NOT roll, or (buggily) roll.
deadline=$(( SECONDS + WINDOW )); gen_after="$gen_before"
while [ "$SECONDS" -lt "$deadline" ]; do
  gen_after="$(sts_generation_sum)"
  st="$(cluster_state)"
  printf '  state=%s sts_generation_sum=%s (baseline %s)\n' "${st:-<none>}" "$gen_after" "$gen_before"
  # converged: back to Ready and generation observed at least once post-restart
  [ "$st" = "Ready" ] && break
  sleep 5
done

rolled="no"; [ "${gen_after:-0}" -gt "${gen_before:-0}" ] && rolled="yes"
# PASS = the live-settable key did NOT cause a roll (generation unchanged) and we are Ready.
verdict="FAIL"
[ "$rolled" = "no" ] && [ "$(cluster_state)" = "Ready" ] && verdict="PASS"

printf '\nREPRODUCED: live_key=%s sts_gen_before=%s sts_gen_after=%s rolled=%s state=%s\nVERDICT: %s\n' \
  "$LIVE_KEY" "${gen_before:-?}" "${gen_after:-?}" "$rolled" "$(cluster_state)" "$verdict"

[ "$verdict" = "PASS" ] || warn "Spurious roll detected (or cluster not Ready): a live-settable key rolled the StatefulSet — this is the bug."
