# slot-migration-interrupt (kuttl TestCase — SKELETON, owner: OPS-8.4, release-gated)

**9.0-only** — drains slots via `CLUSTER MIGRATESLOTS` (Valkey 9.0+). Kill the
destination primary mid-migration and assert no orphaned slots (arch §4.1 row 5).

Flow (authored — OPS-8.4 LEG B):

1. `00`–`01` cert-manager + operator.
2. `02-create-cluster` — apply `conf/cr-cluster.yaml` then patch `spec.shards: 4` so a
   later scale-in (4 → 3) drives a REAL drain of the leaving shard's slots onto a
   surviving destination. `02-assert` waits for the 4-shard cluster Ready.
3. `03-seed-data` — seed 100 keys.
4. `04-start-drain` — patch `spec.shards: 4 → 3` to KICK OFF the drain (one
   `PlanDrainMove` per reconcile, `CLUSTER MIGRATESLOTS SLOTSRANGE … NODE …`, batch 400).
5. `05-kill-dest-mid-migration` — bounded-poll `CLUSTER GETSLOTMIGRATIONS` until a
   migration is IN FLIGHT, parse the destination node id, map id → ip → pod, and delete
   the DESTINATION pod. Deterministic interruption gated on the observable
   GETSLOTMIGRATIONS signal — NOT a sleep (arch §4.2). If no migration is observed
   within the deadline the step FAILS (it does not fake an interruption).
6. `05-assert` — STRICT no-slot-loss invariant: `cluster_slots_assigned:16384` holds
   throughout. Tolerant on the terminal outcome: either the drain RE-PLANS against a
   healthy destination and converges (`state: Ready`, 3 shards, `cluster_state:ok`) OR
   the operator HALTS and surfaces `Degraded`/`Failed` with a reason. NEVER silent slot
   loss, never Ready with < 16384 slots.
7. `99-remove-cluster-gracefully`.

**9.0-only** — atomic `CLUSTER MIGRATESLOTS`/`GETSLOTMIGRATIONS` are Valkey 9.0+
(arch §3.2). `hack/lint-csv.sh` enforces this test stays 9.0-only in the CSV matrices.
