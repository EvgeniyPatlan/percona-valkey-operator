# website/ — k8svalkey-docs MkDocs site source (M7 docs leg)

In-operator scaffold for the **`k8svalkey-docs`** documentation site (MkDocs Material +
`mike` multi-version + `macros`/`variables.yml`), mirroring `k8spxc-docs`/`k8spsmdb-docs`/
`k8sps-docs`/`k8spg-docs`. The canonical site is its own repo; this directory is where the
M7 docs leg (OPS-7.8/7.9) authors and previews before mirroring across.

> `mkdocs` is **absent** in this environment. The leg authors `mkdocs.yml`/`mkdocs-base.yml`/
> `variables.yml`/`main.py`/`requirements.txt` and the `docs/` tree as files; `mkdocs build
> --strict`, `mike deploy`, and publishing are **CI/tool-only** and **human-gated**.

## What this leg delivers (OPS-7.8 / OPS-7.9)

- `mkdocs.yml` with `INHERIT: mkdocs-base.yml`; the base owns `nav`/`plugins`/`extra`
  (`extra.version.provider: mike`).
- `macros` plugin `include_yaml: ["variables.yml"]`; `main.py` function macros
  (`k8svalkeyjira(...)`, `blob(...)`).
- `variables.yml` seeded with `release:`, the `*recommended:` pins, and a `date:` entry.
- `docs/` user-facing pages: `index`, `quickstart`, `install-helm`, `install-olm`,
  `configuration`, `backup-restore`, `upgrades`, `versions` (compatibility matrix), and
  `ReleaseNotes/Kubernetes-Operator-for-Valkey-RN0.1.0.md`.
- `requirements.txt` (mkdocs-material + macros + mike, pinned).
- `CONTRIBUTING-release.md` — the per-release docs checklist.
- The repo-root `.github/workflows/verify-release-tag.yml` gates `release:` against an
  existing `v<release>` git tag.

## Single version-source rule

- **`variables.yml release:`** == `pkg/version/version.txt` (operator axis) — and it **must
  equal an existing operator git tag `v<release>`** or every `blob/v{{release}}/...` link
  404s. Tag the operator **before** publishing docs. (arch 10 §5.1 trap, §6.1 trap 5)
- **`variables.yml *recommended:`** and the `versions.md` matrix == `e2e-tests/release_versions`
  (engine axis). Two more hand-edited copies — keep consistent.
- `mike deploy <version>`; add the `latest` alias **only at GA**, not for `v1alpha1`
  pre-releases (OQ-5).

See `RELEASE.md` (repo root) for the full per-release docs checklist and merge order
(operator -> helm -> docs).
