# Quickstart

This walks you from an empty cluster to a running Valkey in a few commands using Helm.
For the OLM/OperatorHub path see [Install with OLM](install-olm.md).

!!! note "Prerequisites"

    - A Kubernetes cluster (>= {{ kubernetesmin }}) and `kubectl` configured.
    - [Helm](https://helm.sh) 3.x.
    - For TLS: [cert-manager](https://cert-manager.io) {{ certmanagerrecommended }}+ (optional).

## 1. Add the Helm repository

```bash
helm repo add percona https://percona.github.io/percona-helm-charts/
helm repo update
```

## 2. Install the operator

```bash
helm install valkey-operator percona/valkey-operator \
  --namespace valkey-operator --create-namespace
```

This installs the operator Deployment, its RBAC, the `ServiceAccount`, and the CRDs.
By default the operator watches only its own namespace; see
[Install with Helm](install-helm.md#watch-scope) for cluster-wide mode.

## 3. Create a Valkey cluster

```bash
helm install my-valkey percona/valkey-db \
  --namespace valkey --create-namespace \
  --set mode=cluster --set shards=3 --set replicas=1
```

This creates a `PerconaValkeyCluster` CR; the operator reconciles it into a running,
sharded Valkey cluster.

## 4. Watch it come up

```bash
kubectl -n valkey get pvk -w
```

```text
NAME        STATE          READYSHARDS   AGE
my-valkey   initializing   0/3           20s
my-valkey   ready          3/3           90s
```

When `STATE` is `ready` the cluster is serving. `READYSHARDS` shows ready vs desired
shards; `REASON` (in `-o wide`) carries a human-readable status.

## 5. Connect

Use the headless Service the operator creates for the cluster (pod-direct access via the
cluster protocol):

```bash
kubectl -n valkey run -it --rm valkey-cli --image=valkey/valkey:{{ valkeydefaultrecommended }} --restart=Never -- \
  valkey-cli -c -h my-valkey
```

The `-c` flag enables cluster-mode redirection (MOVED/ASK), which is how clients reach the
correct shard.

## 6. Tear down

```bash
helm -n valkey uninstall my-valkey
helm -n valkey-operator uninstall valkey-operator
```

!!! warning

    Uninstalling the `valkey-db` release deletes the `PerconaValkeyCluster` and its pods.
    PersistentVolumeClaims follow the configured `persistence.reclaimPolicy` (default
    `Retain` — data survives). Back up first if the data matters; see
    [Backup and restore](backup-restore.md).

## Where to go next

- [Configuration](configuration.md) — every knob the cluster exposes.
- [Backup and restore](backup-restore.md) — schedule snapshots and restore them.
- [Upgrades](upgrades.md) — operator and engine upgrades on two version axes.
