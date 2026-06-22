# scaling (kuttl TestCase — SKELETON, owner: OPS-8.1)

Scale-out and scale-in of a cluster-mode cluster. **9.0-only** — atomic
`CLUSTER MIGRATESLOTS` / `CLUSTER GETSLOTMIGRATIONS` are Valkey 9.0+ (arch §3.2).

Model on `../init-cluster/` (NN-step `TestStep` + NN-assert `TestAssert`). Suggested flow:

1. `00`–`02` reuse init-cluster (cert-manager → operator → 3-shard cluster Ready).
2. `03-scale-out` — patch `spec.shards: 3 → 6`; assert even slot distribution
   (~`16384/N` ±1) and one `PlanRebalanceMove` per reconcile (no slot churn).
3. `04-scale-in` — patch `spec.shards` back down; assert `PlanDrainMove` drains the
   leaving shard, `cluster_slots_assigned:16384` holds throughout (no slot loss).
4. `99-remove` — graceful teardown.

Asserts grep `cluster_state:ok` + `cluster_slots_assigned:16384` (the real
`CLUSTER INFO` fields) and the CR `status.state: Ready`. See arch §3, §5.
