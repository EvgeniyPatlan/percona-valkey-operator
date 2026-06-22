# e2e-tests/ — kuttl end-to-end suite + engine matrix

End-to-end tests for the Percona Operator for Valkey, in the **kuttl** style (matching the
PS/PG operators; PXC/PSMDB use the older bash harness — we deliberately chose kuttl for
declarative, golden-style assertions). See `docs/architecture/11-testing-qa.md` §3.

> **These do not run on a PR.** GitHub Actions runs unit + lint only and never provisions
> a database cluster. This suite needs a LIVE cluster and a BUILT/PUSHED operator image:
> Jenkins runs it on GKE, and `make e2e-test` runs it on a local Kind.

## Layout

```
e2e-tests/
  kuttl.yaml          # kuttl TestSuite config (testDirs, timeout, parallel)
  functions           # shared bash sourced by script steps (deploy_*, apply_cluster, …)
  conf/               # CR templates (cr-cluster.yaml, cr-replication.yaml) — envsubst-rendered
  release_versions    # SINGLE SOURCE OF TRUTH for engine/sidecar image tags (release axis)
  vars.sh             # CERT_MANAGER_VER (synced from go.mod by hack/release.sh)
  run-pr.csv          # smoke matrix      (per-PR gate)
  run-distro.csv      # full cross-engine / cross-distro matrix
  run-minikube.csv    # local / minikube-runnable subset
  run-release.csv     # release validation (full matrix + failover/chaos)
  tests/              # the TestCases (NN-step / NN-assert) — see tests/README.md
```

## Running

```bash
# one case on a local Kind, against a built+loaded operator image
make e2e-test TEST=init-cluster IMAGE=perconalab/valkey-operator:main

# the whole suite (all dirs under tests/)
make e2e-test IMAGE=perconalab/valkey-operator:main
```

`make e2e-test` wraps `kubectl kuttl test --config e2e-tests/kuttl.yaml` (and, PS-style,
runs `kuttl-shfmt` first when available). **kuttl is not installed in every dev
environment** — the target guards for it and prints install instructions if missing
(it is provided on the Jenkins/CI runners). A live `kubectl` context is required.

## The `run-*.csv` matrix mechanism

Test selection and engine-version fan-out are **data**, not hard-coded lists. Each CSV row
is:

```
test-name,valkey-version
```

- **`test-name`** MUST be a directory under `tests/`.
- **`valkey-version`** is the engine major (`7.2` | `8.0` | `9.0`); the same test runs
  once per row, so a test fans out across engine versions by adding rows.
- Lines starting with `#` and blank lines are ignored.

The four files are four **suites**: `run-pr.csv` (smoke / per-PR), `run-distro.csv` (full
matrix), `run-minikube.csv` (local subset), `run-release.csv` (release + chaos). The
Jenkinsfile reads the appropriate CSV and runs the listed cases in parallel across cluster
suffixes.

### Engine-version gating (mandatory)

`docs/architecture/11-testing-qa.md` §3.2: migration/scale tests (`scaling`,
`slot-migration-interrupt`, any scale-in drain) are **9.0-only** — atomic
`CLUSTER MIGRATESLOTS` / `CLUSTER GETSLOTMIGRATIONS` require Valkey 9.0+. On 7.2/8.0 the
subcommand is `unknown subcommand` and the operator (correctly) blocks scale, so those
rows would fail. 7.2/8.0 rows are limited to cluster-mode basics (`init-cluster`, `tls`,
`acl-users`) where bootstrap uses `CLUSTER ADDSLOTSRANGE` (the 7.x floor).

### CSV lint — kill the silent-skip footgun

A typo in a `test-name` or `valkey-version` would silently drop the row (false green).
`hack/lint-csv.sh` turns each into a hard failure:

```bash
hack/lint-csv.sh          # lint all run-*.csv  (also wired into `make e2e-test`)
```

It checks: (1) every `test-name` is a real `tests/<name>/` dir, (2) the version is in
`{7.2, 8.0, 9.0}`, (3) no migration/scale test is listed below 9.0.

## References

- `docs/architecture/11-testing-qa.md` — pyramid, kuttl, CSV matrix, chaos, CI gates.
- `docs/implementation/09-phase8-testing-qa.md` — the M8 task breakdown (OPS-8.x).
- `tests/README.md` — the NN-step/NN-assert convention and golden-compare approach.
