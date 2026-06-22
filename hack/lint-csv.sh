#!/usr/bin/env bash
# hack/lint-csv.sh — lint the e2e-tests/run-*.csv engine matrices.
#
# Kills the "silent skip" footgun (docs/architecture/11-testing-qa.md §3.2, §8.5):
# a typo in a test-name or version column would otherwise be dropped on the floor by
# the older bash harnesses and produce a false green. This turns each into a hard fail:
#   1. every row's test-name is a real directory under e2e-tests/tests/
#   2. the version column is in the allowed engine set (7.2 | 8.0 | 9.0)
#   3. any test that REBALANCES or DRAINS slots is 9.0-only
#      (CLUSTER MIGRATESLOTS / CLUSTER GETSLOTMIGRATIONS are Valkey 9.0+; on 7.2/8.0
#       the subcommand is "unknown subcommand" and the operator blocks scale).
#
# Usage: hack/lint-csv.sh            (lints all e2e-tests/run-*.csv)
#        hack/lint-csv.sh path.csv   (lints one file)
# Exit 0 = clean, non-zero = at least one violation (rows printed to stderr).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tests_dir="$repo_root/e2e-tests/tests"

# Tests whose name implies they rebalance/drain slots → 9.0-only. failover-takeover
# is a persistence-OFF TAKEOVER and does NOT migrate slots, so it is intentionally
# NOT in this set (arch §8.5).
migration_tests='scaling|slot-migration-interrupt'
# Allowed engine majors (cluster-mode + ACL floor is 7.x).
allowed_versions='7\.2|8\.0|9\.0'

fail=0
csvs=()
if [ "$#" -gt 0 ]; then
  csvs=("$@")
else
  for f in "$repo_root"/e2e-tests/run-*.csv; do csvs+=("$f"); done
fi

for csv in "${csvs[@]}"; do
  [ -f "$csv" ] || { echo "MISSING csv file: $csv" >&2; fail=1; continue; }
  lineno=0
  # ver="" default so `set -u` is safe on comment/blank lines; %$'\r' strips a
  # trailing CR for CRLF-committed CSVs.
  while IFS=, read -r test ver _rest || [ -n "$test" ]; do
    lineno=$((lineno + 1))
    test="${test%$'\r'}"; ver="${ver%$'\r'}"
    # trim surrounding whitespace
    test="${test#"${test%%[![:space:]]*}"}"; test="${test%"${test##*[![:space:]]}"}"
    ver="${ver#"${ver%%[![:space:]]*}"}";   ver="${ver%"${ver##*[![:space:]]}"}"
    [ -z "$test" ] && continue
    case "$test" in \#*) continue ;; esac

    if [ ! -d "$tests_dir/$test" ]; then
      echo "MISSING test dir: '$test' ($csv:$lineno) — no e2e-tests/tests/$test" >&2; fail=1
    fi
    if ! [[ "$ver" =~ ^($allowed_versions)$ ]]; then
      echo "BAD engine version: '$ver' for '$test' ($csv:$lineno) — allowed: 7.2|8.0|9.0" >&2; fail=1
    fi
    if [[ "$test" =~ ^($migration_tests)$ && "$ver" != "9.0" ]]; then
      echo "ILLEGAL: '$test' must be 9.0-only (MIGRATESLOTS is Valkey 9.0+) ($csv:$lineno)" >&2; fail=1
    fi
  done < "$csv"
done

if [ "$fail" -eq 0 ]; then
  echo "csv-lint: all run-*.csv rows OK"
fi
exit "$fail"
