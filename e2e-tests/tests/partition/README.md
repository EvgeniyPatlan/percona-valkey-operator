# partition (kuttl TestCase — SKELETON, owner: OPS-8.4, release-gated)

Network partition isolating one shard's primary from the majority (arch §4.1 row 4).
The operator must NOT issue TAKEOVER while a quorum-holding majority of slot-owning
primaries is reachable (avoid split-brain) — native election handles it.

Flow (authored — OPS-8.4 LEG B):

1. `00`–`02` cert-manager → operator → durable 3-shard cluster Ready; `03-seed-data`.
2. `04-partition` — resolve the current shard-0 primary, stamp a unique chaos label, and
   apply a deny-all (ingress+egress empty) `NetworkPolicy` selecting that label.
   `04-assert` checks the policy is active AND `ReplicasTakenOver` is NOT emitted while
   quorum holds (no split-brain TAKEOVER).
3. `05-heal` — delete the NetworkPolicy and the chaos label; the isolated primary rejoins
   (persistence ON → same node ID, reclaims slots).
4. `05-assert` — after heal: exactly 3 slot-owning primaries (one per shard, no
   split-brain duplicate slot ownership), `cluster_state:ok`, all 16384 slots intact,
   data preserved, `status.state: Ready`.
5. `99-remove-cluster-gracefully`.

Fault injection: NetworkPolicy (arch §4.2). No TAKEOVER must be observed during the
partition while quorum holds.

**CNI requirement:** NetworkPolicy enforcement needs a policy-aware CNI (Calico/Cilium).
The default kind CNI (kindnet) does NOT enforce policies, so this case is meaningful only
on Jenkins/GKE or a Calico-equipped kind. On a non-enforcing CNI the partition is a no-op
and the asserts still pass (the cluster was never actually disrupted) — run it where the
CNI enforces, or treat a kindnet pass as inconclusive.
