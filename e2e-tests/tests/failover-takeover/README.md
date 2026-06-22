# failover-takeover (kuttl TestCase — SKELETON, owner: OPS-8.4)

Quorum LOST, persistence OFF (cache mode, `workloadType: Deployment`): the ONLY case
where the operator itself issues `CLUSTER FAILOVER TAKEOVER` — arch §4.1 row 2.

This is the plan-introduced split of the arch §4.1 quorum-lost kill-primary scenario
(OPEN QUESTION #7): ship as a 2nd dir OR a parametrized variant of failover-kill-primary;
both satisfy the §4.1 assertions.

Flow (authored — OPS-8.4 LEG B):

1. `00`–`01` cert-manager + operator.
2. `02-create-cluster` — apply the LOCAL cache-mode template `conf/cr-cache.yaml`
   (`workloadType: Deployment`, NO `persistence`, `shards: 1`, `replicas: 1`) so a
   single slot-owning primary means `HasFailoverQuorum()` is false on kill AND there is
   no persisted node ID to reclaim. `02-assert` waits for `state: Ready`.
3. `03-seed-data` — seed a fixed keyspace (proves no slot/data loss after promotion).
4. `04-kill-primary` — resolve the CURRENT primary from `CLUSTER NODES` (cache-mode pods
   are Deployment-backed, NOT `valkey-cluster1-0-0`) and delete it deterministically.
5. `04-assert` — `promoteOrphanedReplicas` runs because `HasFailoverQuorum()==false` AND
   `spec.persistence==nil`: picks `BestReplicaOf` (highest `slave_repl_offset`), issues
   `CLUSTER FAILOVER TAKEOVER` (TAKEOVER BEFORE `CLUSTER FORGET` so slots stay owned).
   Assert `cluster_state:ok`, `cluster_slots_assigned:16384`, `status.state: Ready`,
   no slot loss, seeded data intact.
6. `99-remove-cluster-gracefully` — finalizer teardown.

**Event-name caveat (OPEN QUESTION #10):** data-plane §286 + testing-qa §4.1 say this
path emits `ReplicasTakenOver` (NOT `FailoverInitiated`); control-plane §67 says
`FailoverInitiated`. Until the docs are reconciled, the assert accepts EITHER so it is
not brittle to the resolution (see `03-assert.yaml`).
