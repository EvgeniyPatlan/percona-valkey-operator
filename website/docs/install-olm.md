# Install with OLM / OperatorHub

The Percona Operator for Valkey ships an [Operator Lifecycle Manager](https://olm.operatorframework.io)
(OLM) bundle and catalog, so it can be installed from OperatorHub or via a `Subscription`
on any OLM-enabled cluster (e.g. OpenShift).

!!! note "Channels"

    | Channel | Purpose |
    |---------|---------|
    | **`candidate`** | Pre-release / RC builds for validation. |
    | **`fast`** | Latest GA, early adopters. |
    | **`stable`** | Conservative GA, default for production. |

    While the API is at `v1alpha1`, only the **`candidate`** channel is populated; `fast`
    and `stable` open at GA.

## Install from OperatorHub (web console)

1. In the OperatorHub catalog, search for **Percona Operator for Valkey**.
2. Click **Install**, choose the **`candidate`** channel and the target namespace.
3. Wait for the operator's `ClusterServiceVersion` (CSV) to report `Succeeded`.

## Install via a Subscription

```yaml
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: percona-valkey-operator
  namespace: operators
spec:
  channel: candidate
  name: percona-valkey-operator
  source: operatorhubio-catalog
  sourceNamespace: olm
```

```bash
kubectl apply -f subscription.yaml
kubectl -n operators get csv -w
```

When the CSV reaches `Succeeded`, create a `PerconaValkeyCluster` — see
[Configuration](configuration.md). The CR API is identical regardless of install method.

## Verifying a catalog before install

A freshly built catalog can be validated as a `CatalogSource` against an OLM-running kind
cluster before submission to OperatorHub. This is a release-engineering step; end users
consume the published OperatorHub listing.

## Image used

The CSV references the GA operator image `percona/valkey-operator:{{ release }}`. The
engine, backup, and exporter images are pinned in the cluster CR exactly as with the Helm
install; see [Versions compatibility](versions.md).
