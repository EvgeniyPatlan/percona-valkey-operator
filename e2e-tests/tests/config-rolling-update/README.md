# config-rolling-update (kuttl TestCase — SKELETON, owner: OPS-8.1/8.3)

Prove the config-hash → rolling-restart discipline (arch §2.2 item 5, §3.3) end-to-end:

1. `00`–`02` reuse init-cluster (cluster Ready).
2. `03-change-restart-key` — patch a RESTART-requiring `spec.config` key (e.g.
   `appendonly`); assert a one-at-a-time rolling restart (StatefulSet generation bumps,
   replicas-before-primary, proactive failover before rolling a primary).
3. `03b-change-live-key` — patch a LIVE-settable key (`maxmemory`); assert NO pod roll
   (StatefulSet generation unchanged) and the value applied via `CONFIG SET`.
4. `04-compare` — `compare_kubectl statefulset/...` and `compare_kubectl configmap/...`
   against `compare/*.yml` goldens (this case anchors the golden-file machinery).

DISCIPLINE: when an intended change alters a generated manifest, REGENERATE the golden
(`compare_kubectl ... --save`) — never edit the test to paper over a real diff (arch §3.3).

Golden files: `compare/statefulset_valkey-cluster1-0-0.yml`,
`compare/configmap_valkey-cluster1.yml` (+ `-90`/`-80` engine variants).
