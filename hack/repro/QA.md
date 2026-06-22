# QA verification runbook (TEMPLATE) — `repro-K8SVK-<n>`

How QA verifies a fix. Three escalating levels; **Level A is mandatory** before sign-off.
Mirrors `repro-K8SPS-732` QA.md and docs/architecture/11-testing-qa.md §8.

Branch under test: `<fix-branch>` (in `../percona-valkey-operator`).

---

## Level A — Unit / envtest (mandatory, fast, no cluster)

```bash
cd ../percona-valkey-operator
git checkout <fix-branch>
# the specific regression test that encodes the bug:
go test ./pkg/controller/perconavalkeycluster/... -run <TestThatEncodesTheBug> -v -ginkgo.v
make test          # full unit+envtest suite must stay green
```

**PASS criteria**
- the new regression test **PASSES** on the fix branch;
- sibling tests for *other modes* (e.g. `replication` while fixing `cluster`) still **PASS**
  (proves no cross-mode regression);
- `make test` exits 0; coverage stays **≥ 80%** (`make cover`).

The regression test, run against `main` *before* the fix, must **FAIL**; against the fix
branch, must **PASS** — that is the proof the test actually guards the regression.

---

## Level B — Mechanism reproduction (recommended, ~10–15 min, Kind)

```bash
export PATH="$HOME/.local/bin:$PATH"
cd repro-K8SVK-<n>
./00-install-tools.sh        # one-time
./01-setup.sh                # wait for "cluster ready"
./02-reproduce.sh            # before/after demonstration
```

**PASS criteria** — `02-reproduce.sh` ends with a `REPRODUCED:`/`VERDICT` block showing the
failing condition in the BEFORE phase and its absence in the AFTER phase. If a run is
timing-inconclusive (the cluster self-healed before observation), re-run with a longer
window: `WINDOW=180 ./02-reproduce.sh`.

---

## Level C — Full e2e with the patched operator (optional, ~20+ min, needs Go + Docker)

```bash
cd repro-K8SVK-<n>
./04-deploy-patched-operator.sh        # build fix image, kind load, redeploy
# Exercise the real scenario (e.g. repeatedly kill the CURRENT primary of shard 0 —
# resolve it from CLUSTER NODES each iteration, never hard-code -0-0).
```

**PASS criteria** — after each fault the cluster returns to `state: Ready` with all 16384
slots assigned and one primary per shard; operator logs show no persistent error loop.

---

## Evidence to attach for sign-off

1. **Level A:** `go test` output (regression test PASS) + `make test` summary + coverage.
2. **Level B:** the `VERDICT`/`REPRODUCED:` block from `02-reproduce.sh`.
3. **Level C (if run):** CR/pod state after repeated faults; relevant operator log excerpt.
4. **CI:** green GitHub Actions (unit, lint, `check-generate`, `gosec`) and, for
   release-blocking fixes, the relevant Jenkins `kuttl` test green on GKE.
