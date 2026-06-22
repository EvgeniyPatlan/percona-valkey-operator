# quorum-loss (kuttl TestCase — SKELETON, owner: OPS-8.4, NEGATIVE, release-gated)

NEGATIVE case (arch §4.1 row 6). Kill a primary AND all its replicas → TAKEOVER is
impossible (no replica to promote) → a documented stuck state.

Flow (authored — OPS-8.4 LEG B):

1. `00`–`01` cert-manager + operator.
2. `02-create-cluster` — apply the LOCAL cache-mode template `conf/cr-cache.yaml`
   (`workloadType: Deployment`, NO `persistence`, `shards: 1`, `replicas: 1`). Cache mode
   is deliberate: a respawned pod returns as a brand-new node with NO slots (no
   `nodes.conf` to reclaim), so the outage is genuinely permanent rather than self-healed
   by a StatefulSet restart. `02-assert` waits for Ready (proves the later Degraded is the
   fault, not a bootstrap failure).
3. `03-kill-all` — delete EVERY valkey pod of the cluster (the only shard's primary AND
   its replica). TAKEOVER is impossible — no replica to promote (arch §4.1 row 6).
4. `03-assert` — the operator must surface `status.state: Degraded` (or `Failed`) and MUST
   NEVER silently mark `Ready` with < 16384 slots assigned (hard-fail on the silent-Ready
   bug). A Degraded reason pointing at manual intervention is expected.
5. `99-remove-cluster-gracefully` (force-fallback teardown for the stuck shard).

This is the headline "negatives never report Ready" guard. Runbook recovery steps are
documented in the GA-readiness doc / QA runbook.
