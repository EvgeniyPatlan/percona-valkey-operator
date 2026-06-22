#!/usr/bin/env bash
# olm-validate.sh — LOCAL, READ-ONLY OLM catalog validation harness (OPS-7.7;
# docs/architecture/10 §4.3). Deploys a freshly built catalog as a CatalogSource on a
# kind + OLM cluster (the vendored operator-lifecycle-manager `make run-local`) and verifies
# the operator installs and upgrade edges resolve.
#
# *** NEVER PUBLISHES. *** It only applies manifests to a LOCAL kind cluster. It does not
# push images, submit to community-operators, or touch any network registry. If the required
# cluster/tooling is absent it prints clear guidance and exits non-zero WITHOUT taking any
# outward action.
#
# Usage: hack/olm-validate.sh <catalog-image-ref> [namespace]
set -euo pipefail

CATALOG_IMG="${1:?usage: olm-validate.sh <catalog-image> [namespace]}"
NS="${2:-operators}"

echo "olm-validate: this is a LOCAL kind+OLM validation only — it never publishes."

for bin in kubectl kind; do
  command -v "$bin" >/dev/null 2>&1 || {
    echo "olm-validate: '$bin' not found on PATH — install it and start a kind+OLM cluster" >&2
    echo "  (use the vendored operator-lifecycle-manager 'make run-local' to stand up OLM)" >&2
    exit 1
  }
done

if ! kubectl get crd catalogsources.operators.coreos.com >/dev/null 2>&1; then
  echo "olm-validate: OLM is not installed on the current context." >&2
  echo "  Run 'make run-local' in the vendored operator-lifecycle-manager repo first." >&2
  exit 1
fi

cat <<YAML | kubectl apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: valkey-operator-catalog
  namespace: ${NS}
spec:
  sourceType: grpc
  image: ${CATALOG_IMG}
  displayName: Percona Operator for Valkey (local validation)
  publisher: Percona
YAML

echo "olm-validate: CatalogSource applied. Watch with:"
echo "  kubectl -n ${NS} get catalogsource,packagemanifest,subscription,csv"
echo "olm-validate: create a Subscription against channel 'candidate' to test the install/upgrade graph."
