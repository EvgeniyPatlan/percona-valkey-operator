# Releasing (so end users can install & test it)

A release publishes **two container images** and a **GitHub Release** whose attached
manifests point at those images. After it, anyone can install the operator with one
`kubectl apply` and follow [scenarios-and-verification.md](scenarios-and-verification.md).

There are two ways to cut a release — pick one. Both produce the same result.

---

## Option A — One click in GitHub Actions (recommended)

No local tooling, multi-arch images, auth via the built-in token.

1. GitHub → **Actions** → **cut-release** → **Run workflow**.
2. Enter the **version** (e.g. `0.1.0`), leave **make_latest** checked, **Run**.

It builds + pushes `ghcr.io/<you>/valkey-operator:<version>` and
`ghcr.io/<you>/valkey-backup:<version>`, then creates Release **v<version>** with
`bundle.yaml`, `cw-bundle.yaml`, and `cr-minimal.yaml` attached.

> **One-time, first release only:** the GHCR packages are created **private**. Make them
> public so users can pull: GitHub → your profile → **Packages** → `valkey-operator` and
> `valkey-backup` → **Package settings** → **Change visibility** → **Public**. (You can also
> link each package to this repo from the same page.)

---

## Option B — One command locally

Use this if you'd rather build on your machine (or push to Docker Hub instead of GHCR).

Prerequisites: `git`, `docker` (with **buildx** for multi-arch), `gh` logged in, a clean
working tree.

```bash
# GHCR (default), multi-arch, full release:
VERSION=0.1.0 ./hack/cut-release.sh

# Preview without pushing/tagging anything:
VERSION=0.1.0 ./hack/cut-release.sh --dry-run

# amd64 only (no buildx needed):
VERSION=0.1.0 ./hack/cut-release.sh --single-arch

# Docker Hub instead of GHCR:
VERSION=0.1.0 REGISTRY=docker.io OWNER=<your-dockerhub-user> ./hack/cut-release.sh
#   (run `docker login` first for Docker Hub)
```

The script: builds + pushes both images, renders pinned manifests into `./dist`
(without modifying any tracked file), then creates the tag + GitHub Release with them
attached. Re-runnable; refuses if the tag/release already exists.

---

## What end users do after a release

```bash
# install the operator + CRDs
kubectl apply --server-side -f \
  https://github.com/<you>/percona-valkey-operator/releases/download/v0.1.0/bundle.yaml

# create a 3-shard test cluster
kubectl apply -f \
  https://github.com/<you>/percona-valkey-operator/releases/download/v0.1.0/cr-minimal.yaml
```

Then [scenarios-and-verification.md](scenarios-and-verification.md) walks through everything
they can do and how to verify it.

---

## What gets published / what's already public

| Image | Built here? | Notes |
|-------|-------------|-------|
| `…/valkey-operator:<version>` | **yes** (`Dockerfile`) | the operator/manager. Required. |
| `…/valkey-backup:<version>` | **yes** (`Dockerfile.sidecar`) | backup helper. Only needed to test backup/restore (set `spec.backup.image`). |
| `percona/valkey:9.1.0` | no | the Valkey engine — already public on Docker Hub. |

---

## Notes & relationship to the other release tooling

- **Versioning.** `pkg/version/version.txt` is `0.1.0` and `deploy/cr.yaml` `crVersion` is
  `0.1`. The `cut-release` path does not edit tracked files; it pins images only in the
  attached artifacts. To also bump the in-tree version/`crVersion` and the engine/backup
  image pins in `deploy/cr*.yaml` (the Percona-style in-place flow), use `hack/release.sh`
  / the `make release` target — that's a separate, heavier path for a "GA" cut.
- **Existing workflows.** `release.yml` / `publish.yml` target the Percona org's Docker Hub
  and are gated for that use; `cut-release.yml` is the self-contained personal/GHCR path and
  needs no secrets. They don't interfere.
- **Helm / OLM.** `charts/` and `bundle/` exist for Helm- and OperatorHub-based installs;
  publishing those is optional and not required for `kubectl apply` testing.
