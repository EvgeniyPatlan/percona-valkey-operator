# e2e-tests/tests/ — kuttl TestCases

This directory holds the [kuttl](https://kuttl.dev/) end-to-end suite. Each subdirectory
is one **TestCase**; the suite is configured by [`../kuttl.yaml`](../kuttl.yaml) and run
via `make e2e-test` (see the repo Makefile and [`../README.md`](../README.md)).

> **Status:** M8 ships the SCAFFOLD. `init-cluster/` is the fully-fleshed reference case;
> the other directories carry a `README.md` describing the steps/asserts the legs
> (OPS-8.1 / OPS-8.4) author against the same pattern. Nothing here runs on a PR — kuttl
> needs a live cluster + a built operator image. GitHub Actions runs unit/lint only;
> Jenkins runs this suite on GKE, and `make e2e-test` runs it on a local Kind.

## The NN-step / NN-assert convention

A TestCase is a sequence of numbered files. kuttl orders them by the `NN` prefix and, for
each step, applies/runs the `TestStep`, then polls the paired `TestAssert` until it
matches or times out.

```
tests/<case>/
  00-<action>.yaml      # kind: TestStep   — apply manifests OR run commands:/script:
  00-assert.yaml        # kind: TestAssert — desired PARTIAL state of named objects
  01-<action>.yaml
  01-assert.yaml
  ...
  99-remove-*.yaml      # graceful teardown (exercise finalizers before namespace GC)
  compare/              # golden files for compare_kubectl (where used)
```

- **`TestStep`** either applies YAML or runs a `commands:`/`script:` block that
  `source ../../functions` to use the shared vocabulary (`deploy_operator`,
  `deploy_cert_manager`, `apply_cluster`, `exec_valkey`, `compare_kubectl`, `wait_*`).
- **`TestAssert`** is itself a golden assertion: kuttl diffs the live object against the
  embedded expected (partial) spec/status and retries until match or the file's
  `timeout:` (an INTEGER number of seconds). An assert may also carry a `commands:`/
  `script:` block that probes the engine (e.g. greps `CLUSTER INFO`).
- `$NAMESPACE` is injected per-TestCase by kuttl; the shared `functions` default it for
  manual runs.

## Golden-file (`compare`) approach

kuttl's `TestAssert` is a partial-spec golden. For richer rendered-manifest checks (the
full StatefulSet a `ValkeyNode` produces, the rendered `valkey.conf` ConfigMap, the
generated ACL Secret) we add a `compare`-style step: `compare_kubectl <kind/name>`
captures the live YAML, **normalizes volatile fields** (`timestamp`, `resourceVersion`,
`uid`, image digests, generated suffixes), and diffs it against
`tests/<case>/compare/<kind>_<name>${SUFFIX}.yml` — with platform/engine variants
selected by suffix (`-90`/`-80`/`-72` by engine, `-oc` OpenShift, `-eks`).

**Discipline (arch §3.3):** when an INTENDED change alters a generated manifest,
**regenerate** the golden — never edit the test logic to paper over a real diff. An
unintended diff is a bug to fix.

## Which case runs where

Selection + engine fan-out are data in [`../run-*.csv`](../) — see
[`../README.md`](../README.md) for the CSV mechanism and the engine-gating rule
(migration/scale tests are 9.0-only). `hack/lint-csv.sh` guards every CSV row against
the silent-skip footgun.

## References

- `docs/architecture/11-testing-qa.md` §3 (kuttl layout), §3.3 (golden compare), §4 (chaos)
- `docs/implementation/09-phase8-testing-qa.md` OPS-8.0 … OPS-8.4
