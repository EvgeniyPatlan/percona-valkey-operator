# Kind reproduction harness (TEMPLATE) — `hack/repro/`

A self-contained, laptop-sized (single-node **Kind**) before/after reproduction harness for
a reported `percona-valkey-operator` issue, modelled exactly on the in-tree
`repro-K8SPS-732` pattern (`lib.sh` + numbered idempotent scripts). It gives QA and
engineers a deterministic before/after demonstration of a root cause without a cloud
cluster. (docs/architecture/11-testing-qa.md §7)

> **This is the TEMPLATE.** Per reported issue, copy this directory to a top-level
> `repro-K8SVK-<n>/` (e.g. `repro-K8SVK-42/`), then adapt `cr-cluster.yaml` and the
> "force the failing condition" block of `02-reproduce.sh` to that bug.

## Files

| File | Purpose |
|------|---------|
| `lib.sh` | Shared config + helpers (sourced by every script): cluster/ns/CR names, `k`/`kk` wrappers, `valkey_ready_count`, `wait_valkey_ready`, `cluster_state`, `wait_for_state`. |
| `00-install-tools.sh` | Download `kind`/`kubectl`/`yq` into `~/.local/bin` (no sudo), idempotent. |
| `01-setup.sh` | Kind cluster + cert-manager + operator bundle + CR, wait `state=Ready`. Idempotent. |
| `02-reproduce.sh` | The deterministic before/after. Ships a **worked reference** (shape (b): the config-hash spurious-roll invariant — a live-settable key must not roll a StatefulSet), adaptable to shape (a) (quorum-loss TAKEOVER). Ends with a machine-greppable `REPRODUCED:`/`VERDICT` line. |
| `03-teardown.sh` | `kind delete cluster` (`--tools` also removes binaries, `--images` prunes images). |
| `04-deploy-patched-operator.sh` | (optional) build the fix image, `kind load`, redeploy — exercise the REAL operator. |
| `cr-cluster.yaml` | The `PerconaValkeyCluster` under test. |

## Run

```bash
export PATH="$HOME/.local/bin:$PATH"
./00-install-tools.sh        # one-time
./01-setup.sh                # wait for "cluster ready"
./02-reproduce.sh            # before/after demonstration → VERDICT: PASS|FAIL
./03-teardown.sh             # (--tools / --images to also clean those)
```

## Gotchas (carry these forward)

- **Namespace ≠ ccTLD.** Never name the namespace so a per-pod short FQDN ends in a real
  ccTLD — CoreDNS forwards it upstream and returns bogus answers. `lib.sh` uses
  `valkey-repro`; always resolve full `…svc.cluster.local` names in probes.
- **Resources.** A single-node Kind needs roughly **4 CPU / 6 GB RAM / 6 GB disk** to pull
  the Valkey + exporter images and form a 3-shard cluster.
- **Current primary, not pod name.** After a failover the primary is no longer pod `-0-0`;
  resolve it each iteration by parsing `CLUSTER NODES` (role comes from the engine). Use the
  `current_primary_pod` helper in `lib.sh` (needs `OPERATOR_PASS`).
- **Pod label selector.** Count/select Valkey server pods by
  `valkey.percona.com/cluster=<cluster>,app.kubernetes.io/component=valkey` (what the operator
  actually stamps). The operator sets `app.kubernetes.io/name=percona-valkey` and overrides
  `app.kubernetes.io/instance` to the *node* name on server pods, so the obvious selectors
  match zero pods — `lib.sh` already uses the correct one (`$VK_POD_SELECTOR`).

See `QA.md` for the sign-off runbook (Level A mandatory / B / C).
