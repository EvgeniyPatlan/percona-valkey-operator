# Versions compatibility

This matrix lists, per operator release, the tested and recommended component versions. It
is hand-maintained and duplicates the operator's pinned GA images and the `*recommended`
values used elsewhere on this site — keep all three consistent (the source of truth is the
operator repo's `e2e-tests/release_versions`).

!!! warning "Use a compatible set"

    All containers in a Valkey pod must be a compatible set — the exporter must understand
    the running engine and the backup tool must match the server's RDB/AOF format. There is
    no version negotiation. Pick a single row; do not mix components across rows.

| Operator | Valkey server (default) | Valkey server (lines) | Backup tool | Exporter | cert-manager | Kubernetes |
|----------|-------------------------|-----------------------|-------------|----------|--------------|------------|
| {{ release }} | {{ valkeydefaultrecommended }} | {{ valkey80recommended }}, {{ valkey90recommended }} | {{ backuprecommended }} | {{ exporterrecommended }} | {{ certmanagerrecommended }}+ | {{ kubernetesmin }}+ |

## Image references

For operator **{{ release }}** the GA images are:

| Component | Image |
|-----------|-------|
| Operator | `percona/valkey-operator:{{ release }}` |
| Server | `percona/percona-valkey:{{ valkeydefaultrecommended }}` |
| Backup | `percona/valkey-backup:{{ backuprecommended }}` |
| Exporter | `percona/valkey-exporter:{{ exporterrecommended }}` |

## Supported platforms

The operator targets standard Kubernetes {{ kubernetesmin }}+ and OLM-enabled distributions
(including OpenShift via the OLM catalog). Container images are published as multi-arch
manifest lists for `linux/amd64` and `linux/arm64`.
