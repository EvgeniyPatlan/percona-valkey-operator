# Docs per-release checklist (k8svalkey-docs)

This encodes the docs leg of the cross-repo release ritual (arch
`docs/architecture/10-distribution-release.md` §5.2). Do it all in **one PR**, and only
**after** the operator is tagged `v<release>` (merge order is operator → helm → docs; arch
10 §8.3). Publishing is human-gated.

## Before you start

- [ ] The operator git tag `v<release>` **exists**. Every `blob/v{{release}}/...` link and
      the `verify-release-tag` CI job depend on it. If the tag is missing, links 404.

## The checklist

1. **`variables.yml`**
   - [ ] Bump `release:` to the new operator version (= `pkg/version/version.txt`).
   - [ ] Update **every** `*recommended:` pin to match the operator's
         `e2e-tests/release_versions` (server lines, backup, exporter, cert-manager,
         platform mins).
   - [ ] Add the new `date:` entry, e.g. `0_1_0: "2026-06-22"`.
2. **Release notes**
   - [ ] Add `docs/ReleaseNotes/Kubernetes-Operator-for-Valkey-RN<version>.md`.
3. **Navigation**
   - [ ] Add that release-notes file to the `Release Notes:` `nav:` in **`mkdocs-base.yml`**
         (newest first) — not in `mkdocs.yml`.
4. **Compatibility matrix**
   - [ ] Add a row to `docs/versions.md` (operator → tested Valkey lines, backup, exporter,
         cert-manager, Kubernetes). This duplicates the `*recommended` values — keep
         consistent.
5. **Publish (human-gated, CI / docs machine only — `mkdocs` is absent locally)**
   - [ ] `pip install -r requirements.txt`
   - [ ] `mkdocs build --strict` (fail on broken refs / unresolved macros)
   - [ ] `mike deploy <version>` — add the `latest` alias **only at GA**
         (`mike deploy <version> latest`), **not** for a `v1alpha1` pre-release (OQ-5).

## Traps (arch 10 §6.1)

- The `*recommended` pins in `variables.yml` **and** the `versions.md` matrix are two
  hand-edited copies of numbers that already live in the operator repo — three places, no
  automation. A drift here is silent.
- A doc-only fix to an **older** release must be published to that version's `mike` branch,
  not just `main`/`latest`.
- Do not confuse the chart `version`/`appVersion` (in `percona-helm-charts`) with the docs
  `release:` — they track the operator version but are bumped in different repos.
