# failover-kill-primary (kuttl TestCase — SKELETON, owner: OPS-8.4)

**Smoke-matrix chaos case** (in run-pr.csv). Quorum HOLDS, persistence ON (the durable
default): kill a primary and assert the operator OBSERVES native election (it does NOT
issue TAKEOVER) — arch §4.1 row 1.

Suggested flow:

1. `00`–`02` reuse init-cluster (durable 3-shard cluster Ready), seed data (`03`).
2. `04-kill-primary` — `kubectl delete pod valkey-cluster1-0-0` (the initial primary).
3. `04-assert` — Valkey's own election promotes the synced replica; the restarted pod
   reloads `nodes.conf`, rejoins with the SAME node ID as a replica. Assert:
   `cluster_state:ok`, `cluster_slots_assigned:16384`, `status.state: Ready`,
   `ClusterFormed=True`, no slot loss, data compare pre/post equal.

Operator must NOT emit a TAKEOVER here — that path is `failover-takeover/` (quorum-lost,
persistence-OFF). Recovery assertions grep `role:master` / `master_link_status:up`
VERBATIM (upstream engine field names; arch §4 note).
