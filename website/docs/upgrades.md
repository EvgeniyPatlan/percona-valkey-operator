# Upgrades

There are **two independent version axes**. Keep them distinct:

| Axis | What it is | Driven by |
|------|-----------|-----------|
| **Operator version** | The operator's own semver (and `spec.crVersion`, the `major.minor`). | The operator image you run + the chart `appVersion`. |
| **Engine version** | The Valkey server image tag (and backup/exporter). | `spec.upgradeOptions` against the version service, or an explicit `spec.image`. |

They move on completely different cadences. A patch operator upgrade does not change the
engine; an engine upgrade does not change the operator.

## `spec.crVersion`

`spec.crVersion` is the operator's `major.minor` (for example `0.1`, never `0.1.0`). It
gates CR API compatibility and upgrade behaviour. Leave it empty and the operator
auto-stamps it from the operator version on first reconcile.

!!! warning "Do not hand-edit `crVersion` to a full semver"

    `crVersion` is `major.minor` only, so patch operator upgrades (`0.1.0 → 0.1.1`) must not
    change it. A `crVersion` that does not match the operator's `major.minor` causes the
    operator to reject or upgrade-loop the CR.

## Upgrading the operator

1. Upgrade the operator (Helm chart upgrade or OLM `Subscription` rolling to a new CSV).
2. The new operator reconciles existing CRs. Within a supported `crVersion` window it
   accepts the existing `crVersion`; a minor operator bump stamps the new `major.minor`.
3. The CRD conversion webhook converts stored `v1alpha1` objects to the served version as
   the API graduates toward `v1` — no manual CR edits required.

=== "Helm"

    ```bash
    helm repo update
    helm -n valkey-operator upgrade valkey-operator percona/valkey-operator
    ```

=== "OLM"

    OLM rolls the `Subscription` to the next CSV in the channel automatically (or on
    approval, depending on the install plan policy).

## Upgrading the engine (smart update)

`spec.upgradeOptions.apply` selects how the engine image is resolved:

| `apply` | Behaviour |
|---------|-----------|
| `Disabled` | Never auto-change the engine image; you pin `spec.image` yourself. |
| `Recommended` | Move to the version-service **recommended** version on the schedule. |
| `Latest` | Move to the **latest** known version on the schedule. |
| `<version>` | Pin to an explicit version (e.g. `{{ valkey90recommended }}`). |

```yaml
upgradeOptions:
  apply: Recommended
  schedule: "0 4 * * *"
  versionServiceEndpoint: https://check.percona.com
```

### Zero-downtime engine roll

When the engine image changes, the operator performs a **failover-aware rolling update**:

1. Replicas are rolled first, one pod at a time.
2. Before rolling a primary, the operator triggers a proactive `CLUSTER FAILOVER` so a
   synced replica takes over with no write interruption (with a `TAKEOVER` fallback only on
   quorum loss).
3. A config-hash annotation ensures pods roll only when a restart-required key actually
   changed.

!!! warning "Compatible set"

    The engine, backup, and exporter images must be a compatible set — there is no version
    negotiation. Bump them together; see [Versions compatibility](versions.md).

## Downgrades

- **Operator / `crVersion`:** forward-only within a supported window.
- **Engine:** forward-only; to run an older engine, restore from a backup into a cluster
  running the target version (see [Backup and restore](backup-restore.md)).

## Version sources behind the docs

The `{{ release }}` value on this site equals the operator version and an existing operator
git tag `v{{ release }}`. The `*recommended` engine pins on [Versions compatibility](versions.md)
mirror the operator's pinned GA images for this release.
