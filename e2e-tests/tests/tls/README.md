# tls (kuttl TestCase — SKELETON, owner: OPS-8.1)

TLS-enabled cluster, both secret-ref and cert-manager modes (arch §7 security). Floor:
cluster mode + TLS works on 7.2+. Suggested flow:

1. `00`–`01` cert-manager + operator.
2. `02-create-cluster-tls` — apply a CR with `spec.tls.certManager.issuerRef` (or a
   pre-created `spec.tls.secretName`); assert the TLS Secret is mounted at
   `/etc/valkey/tls/` and the cluster forms over TLS.
3. `03-verify-tls` — `exec_valkey ... --tls` succeeds; a plaintext probe is refused.

Golden: optionally `compare/` the rendered StatefulSet TLS volume/mount.
