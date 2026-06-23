# Percona Operator for Valkey — Makefile (Percona Operator-SDK trio family).
# See docs/architecture/02-repo-layout.md §4-5 for the target vocabulary.

# ---------------------------------------------------------------------------
# Module / image identity
# ---------------------------------------------------------------------------
MODULE              := valkey.percona.com/percona-valkey-operator

# VERSION FOOTGUN (preserved from the Percona trio): defaults to the sanitised
# branch name. ALWAYS pass VERSION=x.y.z for any release/build action, or images
# get tagged with the branch name and crVersion/version.txt are written wrong.
# (docs/architecture/10-distribution-release.md §1; 02 §5)
VERSION             ?= $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null | tr '/' '-' | tr -cd 'A-Za-z0-9._-')
# GA images publish under percona/; dev/main builds under perconalab/.
IMAGE_TAG_OWNER     ?= perconalab
IMG                 ?= $(IMAGE_TAG_OWNER)/valkey-operator:$(VERSION)

# The four images (operator/server/backup/exporter). All four are buildable here;
# the engine-axis tags are pinned in e2e-tests/release_versions and baked into
# deploy/cr*.yaml by `make release`. (docs/architecture/10 §2)
IMAGE_OPERATOR_REPO ?= $(IMAGE_TAG_OWNER)/valkey-operator
IMAGE_SERVER_REPO   ?= $(IMAGE_TAG_OWNER)/percona-valkey
IMAGE_BACKUP_REPO   ?= $(IMAGE_TAG_OWNER)/valkey-backup
IMAGE_EXPORTER_REPO ?= $(IMAGE_TAG_OWNER)/valkey-exporter
IMAGE_SERVER        ?= $(IMAGE_SERVER_REPO):$(VERSION)
IMAGE_BACKUP        ?= $(IMAGE_BACKUP_REPO):$(VERSION)
IMAGE_EXPORTER      ?= $(IMAGE_EXPORTER_REPO):$(VERSION)
# Valkey engine base image for the sidecar/server build (overridden by the server pipeline).
VALKEY_BASE_IMAGE   ?= valkey/valkey:8-alpine

# Engine-axis source of truth (single edit point — docs/architecture/10 §1, §8).
RELEASE_VERSIONS    ?= e2e-tests/release_versions
# kuttl e2e vars file that receives the synced CERT_MANAGER_VER.
E2E_VARS            ?= e2e-tests/vars.sh

# Embed the operator version into the binary at build time. GO-7.2 also stamps commit/time.
GIT_COMMIT          ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME          ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)
VERSION_LDFLAGS     ?= -X $(MODULE)/pkg/version.gitVersion=$(VERSION) \
                       -X $(MODULE)/pkg/version.GitCommit=$(GIT_COMMIT) \
                       -X $(MODULE)/pkg/version.BuildTime=$(BUILD_TIME)

# Pinned Kubernetes version for envtest assets.
ENVTEST_K8S_VERSION ?= 1.34.1

# Multi-arch target platforms (GA matrix: amd64 + arm64).
PLATFORMS           ?= linux/amd64,linux/arm64

# ---------------------------------------------------------------------------
# Tooling — auto-downloaded into ./bin (gitignored), pinned versions (OPS-0.4)
# ---------------------------------------------------------------------------
LOCALBIN            ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

CONTROLLER_GEN      ?= $(LOCALBIN)/controller-gen
KUSTOMIZE           ?= $(LOCALBIN)/kustomize
ENVTEST             ?= $(LOCALBIN)/setup-envtest
MOCKGEN             ?= $(LOCALBIN)/mockgen
GOLANGCI_LINT       ?= $(LOCALBIN)/golangci-lint
OPERATOR_SDK        ?= $(LOCALBIN)/operator-sdk
OPM                 ?= $(LOCALBIN)/opm

CONTROLLER_GEN_VERSION ?= v0.19.0
KUSTOMIZE_VERSION      ?= v5.7.1
ENVTEST_VERSION        ?= release-0.23
MOCKGEN_VERSION        ?= v0.6.0
GOLANGCI_LINT_VERSION  ?= v2.12.2
# OLM tooling (wired in M7). ABSENT in this environment — the bundle/catalog targets
# guard for them and download into ./bin on a real CI runner; they NEVER push here.
OPERATOR_SDK_VERSION   ?= v1.41.1
OPM_VERSION            ?= v1.55.0
# OS/arch suffixes for the operator-sdk/opm release-binary download URLs.
OS                  ?= $(shell go env GOOS 2>/dev/null || echo linux)
ARCH                ?= $(shell go env GOARCH 2>/dev/null || echo amd64)

# Helm charts live in the sibling percona-helm-charts repo; this tree carries source
# copies under charts/ that legs author and sync. helm-lint/helm-package operate on them.
HELM                ?= helm
CHARTS_DIR          ?= charts

# Project Go version (single source of truth: .go-version). golangci-lint must be
# COMPILED with a Go >= the targeted go directive, so we force its install toolchain
# to this version rather than whatever GOTOOLCHAIN=auto happens to pick.
GO_VERSION          ?= $(shell cat .go-version 2>/dev/null)
GO                  ?= GOTOOLCHAIN=auto go

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	  /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: generate
generate: controller-gen mockgen ## Generate deepcopy + mocks (run after editing *_types.go).
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./pkg/apis/..."

.PHONY: manifests
manifests: controller-gen kustomize ## Generate CRD/RBAC into config/, then render the full deploy/ artifact set.
	$(CONTROLLER_GEN) rbac:roleName=valkey-operator crd webhook paths="./..." \
	  output:crd:artifacts:config=config/crd/bases
	# Render the flat deploy/ install manifests the architecture doc enumerates
	# (docs/architecture/02-repo-layout.md §2, §7). With no CRDs yet (M0) the
	# crd.yaml render is empty/near-empty but the chain is proven.
	$(KUSTOMIZE) build config/crd                  > deploy/crd.yaml
	$(KUSTOMIZE) build config/rbac                 > deploy/rbac.yaml
	$(KUSTOMIZE) build config/manager              > deploy/operator.yaml
	$(KUSTOMIZE) build config/default              > deploy/bundle.yaml
	$(KUSTOMIZE) build config/cluster-wide/rbac    > deploy/cw-rbac.yaml
	$(KUSTOMIZE) build config/cluster-wide/manager > deploy/cw-operator.yaml
	$(KUSTOMIZE) build config/cluster-wide         > deploy/cw-bundle.yaml

.PHONY: fmt
fmt: ## Run go fmt.
	$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet ./...

.PHONY: test
test: manifests generate fmt vet envtest ## Run unit + envtest tests with coverage.
	# Run every package (compile + pass), but emit the coverage profile only for
	# packages that actually contain tests. Profiling across test-less packages
	# forces Go to merge with the `covdata` tool; restricting the profile to
	# tested packages keeps `make test` portable and matches the coverage policy
	# (coverage measured on pkg/... with logic — docs 11 §1). -coverpkg attributes
	# covered lines back to the pkg/ logic packages even from the cmd/manager suite.
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
	  $(GO) test $$($(GO) list ./... | grep -v /e2e-tests) -race
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
	  $(GO) test $$(for p in $$($(GO) list ./pkg/... | grep -v /e2e-tests); do \
	      d=$$($(GO) list -f '{{.Dir}}' $$p); ls $$d/*_test.go >/dev/null 2>&1 && echo $$p; done) \
	      -coverprofile cover.out

.PHONY: cover
cover: ## Coverage gate — recompute pkg/ line coverage and fail below the 80% floor.
	# Re-runs the tested pkg/... packages with a coverage profile, then enforces the
	# floor. Coverage is measured on pkg/... ONLY (excludes cmd/, generated deepcopy,
	# and e2e helpers — docs/architecture/11-testing-qa.md §1). Mirrors the CI gate in
	# .github/workflows/tests.yml so a local `make cover` matches red/green in CI.
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
	  $(GO) test $$(for p in $$($(GO) list ./pkg/... | grep -v /e2e-tests); do \
	      d=$$($(GO) list -f '{{.Dir}}' $$p); ls $$d/*_test.go >/dev/null 2>&1 && echo $$p; done) \
	      -coverprofile cover.out
	@total=$$($(GO) tool cover -func=cover.out | awk '/^total:/ {gsub("%","",$$3); print $$3}'); \
	  echo "pkg coverage: $${total}%"; \
	  awk -v t="$${total}" 'BEGIN { if (t+0 < 80.0) { print "ERROR: pkg coverage " t "% < 80% floor"; exit 1 } }'

.PHONY: check-generate
check-generate: manifests generate ## CRD/deepcopy/RBAC drift gate — fails on a dirty tree.
	@git diff --exit-code || (echo "ERROR: generated artifacts are stale — run 'make generate manifests' and commit"; exit 1)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint with --fix.
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Validate the golangci-lint config.
	$(GOLANGCI_LINT) config verify

##@ E2E & Security

# kuttl binary discovery: prefer ./bin, then a kubectl plugin (`kubectl kuttl`), then a
# bare `kuttl` on PATH. ABSENT in this authoring environment — the target is GUARDED and
# prints install guidance rather than failing opaquely. kuttl runs on the CI/Jenkins
# runners and on a developer Kind, never on a PR. (docs/architecture/11-testing-qa.md §3)
KUTTL               ?= $(LOCALBIN)/kubectl-kuttl
# Selector: `make e2e-test` runs the whole suite; `make e2e-test TEST=<name>` one case.
TEST                ?=
KUTTL_TEST_FLAG     := $(if $(strip $(TEST)),--test $(TEST),)

.PHONY: lint-csv
lint-csv: ## Lint the e2e run-*.csv matrices (missing dir / bad version / migration<9.0).
	./hack/lint-csv.sh

.PHONY: e2e-test
e2e-test: lint-csv ## Run the kuttl e2e suite (TEST=<name> for one case; needs kuttl + a live cluster + IMAGE=<img>).
	# GUARD: kuttl is not installed in every dev environment. Resolve it from ./bin,
	# a kubectl plugin, or PATH; if none is found, print install guidance and stop
	# (NEVER pretend a run happened). A live kubectl context and a built/pushed operator
	# IMAGE are required — this does not run on a PR (GitHub Actions is unit/lint only).
	@kuttl_bin=""; \
	if [ -x "$(KUTTL)" ]; then kuttl_bin="$(KUTTL) test"; \
	elif kubectl kuttl version >/dev/null 2>&1; then kuttl_bin="kubectl kuttl test"; \
	elif command -v kuttl >/dev/null 2>&1; then kuttl_bin="kuttl test"; \
	else \
	  echo "ERROR: kuttl not found."; \
	  echo "  Install: https://kuttl.dev/docs/cli.html  (or 'kubectl krew install kuttl')"; \
	  echo "  CI/Jenkins provides it; it is intentionally absent in this author-only env."; \
	  exit 1; \
	fi; \
	if command -v kuttl-shfmt >/dev/null 2>&1; then \
	  echo "==> kuttl-shfmt (format check)"; kuttl-shfmt -d e2e-tests/tests || exit 1; \
	fi; \
	echo "==> $$kuttl_bin --config e2e-tests/kuttl.yaml $(KUTTL_TEST_FLAG) (IMAGE=$(IMG))"; \
	IMAGE="$(IMG)" $$kuttl_bin --config e2e-tests/kuttl.yaml $(KUTTL_TEST_FLAG)

# Security scanners: gosec (HIGH-severity SAST) + govulncheck (known-vuln check). Both are
# ABSENT here; the target downloads each into ./bin on a real runner and NEVER fails the
# build merely because a tool is missing locally (author-only). HIGH gosec findings DO fail
# on CI (.github/workflows/scan.yml). (docs/architecture/11-testing-qa.md §6.1)
GOSEC               ?= $(LOCALBIN)/gosec
GOVULNCHECK         ?= $(LOCALBIN)/govulncheck
GOSEC_VERSION       ?= v2.27.1
GOVULNCHECK_VERSION ?= latest

.PHONY: scan
scan: scan-gosec scan-vuln ## Run gosec (HIGH-fail) + govulncheck; downloads into ./bin, soft on local absence.

.PHONY: scan-gosec
scan-gosec: $(LOCALBIN) ## gosec SAST — fails only on HIGH findings (matches CI).
	@if [ ! -x "$(GOSEC)" ]; then \
	  echo "==> gosec absent; fetching $(GOSEC_VERSION) into $(LOCALBIN)"; \
	  GOBIN=$(LOCALBIN) $(GO) install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION) \
	    || { echo "WARN: could not install gosec (offline?); skipping locally — CI enforces it"; exit 0; }; \
	fi; \
	echo "==> gosec -severity high ./..."; \
	"$(GOSEC)" -severity high ./...

.PHONY: scan-vuln
scan-vuln: $(LOCALBIN) ## govulncheck known-vulnerability scan (advisory locally).
	@if [ ! -x "$(GOVULNCHECK)" ]; then \
	  echo "==> govulncheck absent; fetching $(GOVULNCHECK_VERSION) into $(LOCALBIN)"; \
	  GOBIN=$(LOCALBIN) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) \
	    || { echo "WARN: could not install govulncheck (offline?); skipping locally — CI runs it"; exit 0; }; \
	fi; \
	echo "==> govulncheck ./..."; \
	"$(GOVULNCHECK)" ./... || echo "WARN: govulncheck reported findings (advisory locally; see scan.yml policy)"

##@ Build

.PHONY: build-manager
build-manager: generate fmt vet ## Build the manager binary into ./bin.
	$(GO) build -ldflags "$(VERSION_LDFLAGS)" -o $(LOCALBIN)/manager ./cmd/manager

.PHONY: run
run: manifests generate fmt vet ## Run the manager off-cluster against the current kube-context (leader election off).
	$(GO) run ./cmd/manager --leader-elect=false

# docker buildx: a multi-arch manifest list cannot be --load-ed into the local
# docker image store, so the multi-platform path runs only when PUSH=true (CI on
# main/tag). Local `make build` produces a single-arch image with --load so
# `docker run … /manager --help` works. (OPS-0.8, docs 10 §2)
.PHONY: build
build: generate ## Build the operator image (single-arch --load; multi-arch --push when PUSH=true).
ifeq ($(PUSH),true)
	docker buildx build --platform $(PLATFORMS) --push \
	  --build-arg VERSION=$(VERSION) -t $(IMG) .
else
	docker buildx build --load \
	  --build-arg VERSION=$(VERSION) -t $(IMG) .
endif

.PHONY: docker-build
docker-build: build ## Alias for `build` (Percona-family vocabulary).

.PHONY: docker-buildx
docker-buildx: build ## Alias for the multi-arch buildx path (`build` is already buildx-based).

# Multi-arch builds for the three DB-side images (server/backup/exporter). Like `build`,
# the multi-arch manifest-list path runs ONLY when PUSH=true (CI on main/tag). PUSH defaults
# to empty so a bare `make build-*` never reaches a registry. (docs/architecture/10 §2)
.PHONY: build-server
build-server: ## Build the Valkey server/sidecar image (single-arch --load; multi-arch when PUSH=true).
ifeq ($(PUSH),true)
	docker buildx build --platform $(PLATFORMS) --push -f Dockerfile.sidecar \
	  --build-arg VALKEY_BASE_IMAGE=$(VALKEY_BASE_IMAGE) -t $(IMAGE_SERVER) .
else
	docker buildx build --load -f Dockerfile.sidecar \
	  --build-arg VALKEY_BASE_IMAGE=$(VALKEY_BASE_IMAGE) -t $(IMAGE_SERVER) .
endif

.PHONY: build-backup
build-backup: ## Build the Valkey backup-tool image (single-arch --load; multi-arch when PUSH=true).
ifeq ($(PUSH),true)
	docker buildx build --platform $(PLATFORMS) --push -f Dockerfile.sidecar \
	  --build-arg VALKEY_BASE_IMAGE=$(VALKEY_BASE_IMAGE) -t $(IMAGE_BACKUP) .
else
	docker buildx build --load -f Dockerfile.sidecar \
	  --build-arg VALKEY_BASE_IMAGE=$(VALKEY_BASE_IMAGE) -t $(IMAGE_BACKUP) .
endif

.PHONY: build-exporter
build-exporter: ## Build the Valkey exporter image (single-arch --load; multi-arch when PUSH=true).
ifeq ($(PUSH),true)
	docker buildx build --platform $(PLATFORMS) --push -f Dockerfile.sidecar \
	  --build-arg VALKEY_BASE_IMAGE=$(VALKEY_BASE_IMAGE) -t $(IMAGE_EXPORTER) .
else
	docker buildx build --load -f Dockerfile.sidecar \
	  --build-arg VALKEY_BASE_IMAGE=$(VALKEY_BASE_IMAGE) -t $(IMAGE_EXPORTER) .
endif

.PHONY: build-all
build-all: build build-server build-backup build-exporter ## Build all four images (operator/server/backup/exporter).

##@ Deployment

.PHONY: install
install: manifests kustomize ## Install CRDs into the current kube-context.
	$(KUSTOMIZE) build config/crd | kubectl apply --server-side -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the current kube-context.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy the operator (namespaced) to the current kube-context.
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default | kubectl apply --server-side -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy the operator from the current kube-context.
	$(KUSTOMIZE) build config/default | kubectl delete --ignore-not-found -f -

.PHONY: deploy-cw
deploy-cw: manifests kustomize ## Deploy the operator cluster-wide (WATCH_NAMESPACE="").
	$(KUSTOMIZE) build config/cluster-wide | kubectl apply --server-side -f -

##@ Tool downloads (into ./bin, gitignored — pinned versions)

.PHONY: controller-gen
controller-gen: $(LOCALBIN) ## Download controller-gen if missing.
	@test -x $(CONTROLLER_GEN) || \
	  GOBIN=$(LOCALBIN) $(GO) install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.PHONY: kustomize
kustomize: $(LOCALBIN) ## Download kustomize if missing.
	@test -x $(KUSTOMIZE) || \
	  GOBIN=$(LOCALBIN) $(GO) install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

.PHONY: envtest
envtest: $(LOCALBIN) ## Download setup-envtest if missing.
	@test -x $(ENVTEST) || \
	  GOBIN=$(LOCALBIN) $(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

.PHONY: mockgen
mockgen: $(LOCALBIN) ## Download mockgen if missing.
	@test -x $(MOCKGEN) || \
	  GOBIN=$(LOCALBIN) $(GO) install go.uber.org/mock/mockgen@$(MOCKGEN_VERSION)

.PHONY: golangci-lint
golangci-lint: $(LOCALBIN) ## Download golangci-lint if missing.
	@test -x $(GOLANGCI_LINT) || \
	  GOBIN=$(LOCALBIN) GOTOOLCHAIN=go$(GO_VERSION) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# operator-sdk / opm are downloaded as release binaries into ./bin on a real CI runner.
# In THIS environment they are absent and have no `go install` path, so the targets print a
# clear message and download via curl. They are GUARDS ONLY — they never push anything.
.PHONY: operator-sdk
operator-sdk: $(LOCALBIN) ## Download operator-sdk into ./bin if missing (no push).
	@if test -x $(OPERATOR_SDK); then \
	  echo "operator-sdk present: $(OPERATOR_SDK)"; \
	else \
	  echo ">> operator-sdk not found — downloading $(OPERATOR_SDK_VERSION) into $(LOCALBIN) (no network publish)"; \
	  curl -fsSL -o $(OPERATOR_SDK) \
	    "https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$(OS)_$(ARCH)" \
	    && chmod +x $(OPERATOR_SDK) \
	    || { echo "ERROR: could not fetch operator-sdk; install it manually into $(OPERATOR_SDK)"; exit 1; }; \
	fi

.PHONY: opm
opm: $(LOCALBIN) ## Download opm into ./bin if missing (no push).
	@if test -x $(OPM); then \
	  echo "opm present: $(OPM)"; \
	else \
	  echo ">> opm not found — downloading $(OPM_VERSION) into $(LOCALBIN) (no network publish)"; \
	  curl -fsSL -o $(OPM) \
	    "https://github.com/operator-framework/operator-registry/releases/download/$(OPM_VERSION)/$(OS)-$(ARCH)-opm" \
	    && chmod +x $(OPM) \
	    || { echo "ERROR: could not fetch opm; install it manually into $(OPM)"; exit 1; }; \
	fi

##@ Release (M7 — the version-stamping & GA-pinning vocabulary)

# crVersion is major.minor ONLY (docs/architecture/10 §6.1 trap 2, §8.1 step 2): a patch
# release (1.1.0 -> 1.1.1) MUST NOT churn crVersion. Computed at parse time so it is visible
# to recipes and to NEXT_VER below.
CRVERSION := $(shell echo "$(VERSION)" | cut -d. -f1-2)

# Reusable footgun guard: abort unless VERSION is a real x.y.z (not a branch name).
# (docs/architecture/10 §1 trap, §6.1 trap 1; impl 08 R2)
define require-semver-version
	@echo "$(VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' \
	  || { echo "ERROR: pass VERSION=x.y.z (got '$(VERSION)' — branch-name footgun; arch 10 §1)"; exit 1; }
endef

.PHONY: release
release: manifests ## GA pinning on a release-x.y.z branch: stamp version.txt + crVersion + percona/* GA image tags (arch 10 §8.1). PASS VERSION=x.y.z.
	$(require-semver-version)
	# version.txt (operator-axis SoT) + crVersion=major.minor + EVERY image -> GA percona/*
	# pulled from e2e-tests/release_versions. --owner percona is load-bearing (NOT perconalab).
	./hack/release.sh \
	  --version $(VERSION) --crversion $(CRVERSION) \
	  --release-versions $(RELEASE_VERSIONS) \
	  --cr deploy/cr.yaml --cr deploy/cr-minimal.yaml \
	  --owner percona
	# Sync cert-manager version into the kuttl e2e vars (arch 10 §8.1 step 3).
	./hack/sync-certmanager.sh go.mod $(E2E_VARS)
	# Regenerate any image-asserting golden fixtures (GO-7.4; never hand-edit).
	$(MAKE) regen-fixtures
	@echo "release: GA pinned VERSION=$(VERSION) crVersion=$(CRVERSION). Review 'git diff', then tag v$(VERSION) (human-approved)."

# NEXT_VER is derived from cr.yaml crVersion (major.minor) as major.(minor+1).0 — NOT from
# version.txt (arch 10 §8.2). Computed at parse time so the update-version PREREQUISITE sees
# it. Override with `make after-release NEXT_VER=x.y.z`.
NEXT_VER ?= $(shell ./hack/next-ver.sh deploy/cr.yaml 2>/dev/null)

.PHONY: after-release
after-release: update-version manifests ## Next dev cycle on main: repoint images to perconalab/*:main-*, bump NEXT_VER (arch 10 §8.2).
	@echo "$(NEXT_VER)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' \
	  || { echo "ERROR: NEXT_VER='$(NEXT_VER)' not x.y.z (check deploy/cr.yaml crVersion or pass NEXT_VER=)"; exit 1; }
	./hack/release.sh \
	  --version $(NEXT_VER) --crversion $(shell echo "$(NEXT_VER)" | cut -d. -f1-2) \
	  --cr deploy/cr.yaml --cr deploy/cr-minimal.yaml \
	  --owner perconalab --dev-tags main
	@echo "after-release: repointed to perconalab/*:main-* for dev cycle NEXT_VER=$(NEXT_VER). NEVER ship GA from this tree (arch 10 §6.1 trap 6)."

.PHONY: update-version
update-version: ## Write NEXT_VER into version.txt ONLY (arch 10 §8.2). Does NOT touch crVersion.
	@echo "$(NEXT_VER)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' \
	  || { echo "ERROR: NEXT_VER='$(NEXT_VER)' not x.y.z"; exit 1; }
	echo "$(NEXT_VER)" > pkg/version/version.txt

.PHONY: regen-fixtures
regen-fixtures: ## Regenerate image-asserting golden fixtures (GO-7.4; deterministic, never hand-edited).
	GO="$(GO)" ./hack/regen-fixtures.sh

# check-version is conceptually owned by M6 OPS-6.3 (shared hack/check-version.sh); defined
# here so `make check-version` works and OPS-7.10's check-version.yml can invoke it.
.PHONY: check-version
check-version: ## Drift gate: version.txt major.minor == deploy/cr.yaml crVersion (arch 10 §6 rec).
	./hack/check-version.sh pkg/version/version.txt deploy/cr.yaml

##@ OLM bundle & catalog (operator-sdk / opm — AUTHOR + LOCAL-VALIDATE ONLY, never push)

IMAGE_TAG_BASE  ?= $(IMAGE_TAG_OWNER)/valkey-operator
BUNDLE_IMG      ?= $(IMAGE_TAG_BASE)-bundle:v$(VERSION)
CATALOG_IMG     ?= $(IMAGE_TAG_BASE)-catalog:v$(VERSION)
CHANNELS        ?= candidate
DEFAULT_CHANNEL ?= candidate
# Channels at v1alpha1 (OQ-3): default to candidate-only until the API graduates to v1;
# override CHANNELS=candidate,fast,stable DEFAULT_CHANNEL=stable for a GA release.
BUNDLE_METADATA_OPTS ?= $(if $(CHANNELS),--channels=$(CHANNELS),) $(if $(DEFAULT_CHANNEL),--default-channel=$(DEFAULT_CHANNEL),)
BUNDLE_IMGS     ?= $(BUNDLE_IMG)
# Incremental catalog: CATALOG_BASE_IMG=<prior-catalog> -> --from-index (arch 10 §4.2).
FROM_INDEX_OPT  := $(if $(CATALOG_BASE_IMG),--from-index $(CATALOG_BASE_IMG),)

.PHONY: bundle
bundle: manifests kustomize operator-sdk ## Generate the OLM bundle (CSV+CRDs+metadata) into ./bundle. PASS VERSION=x.y.z.
	$(require-semver-version)
	$(OPERATOR_SDK) generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMAGE_OPERATOR_REPO):$(VERSION)
	$(KUSTOMIZE) build config/manifests | \
	  $(OPERATOR_SDK) generate bundle --version $(VERSION) $(BUNDLE_METADATA_OPTS)
	$(OPERATOR_SDK) bundle validate ./bundle
	@echo "bundle: generated ./bundle for v$(VERSION) channels='$(CHANNELS)' default='$(DEFAULT_CHANNEL)'. Submission to community-operators is a separate, human-approved step."

.PHONY: bundle-build
bundle-build: ## Build the OLM bundle image locally (NO push; requires PUSH=true to even attempt, which is blocked here).
	@echo "bundle-build: building $(BUNDLE_IMG) locally (no push)."
	docker build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## PUBLISH GUARD — pushing the bundle image requires explicit PUSH=true and human approval.
	@if [ "$(PUSH)" != "true" ]; then \
	  echo "REFUSING to push: set PUSH=true AND obtain human approval (arch: publishing is human-gated)."; exit 1; \
	fi
	@echo "PUSH=true requested for $(BUNDLE_IMG). This is an OUTWARD action — a human operator must run it intentionally."
	docker push $(BUNDLE_IMG)

.PHONY: catalog-build
catalog-build: opm ## Build the OLM catalog (index) image locally via opm (NO push). PASS VERSION=x.y.z.
	$(require-semver-version)
	$(OPM) index add --mode semver --tag $(CATALOG_IMG) --bundles $(BUNDLE_IMGS) $(FROM_INDEX_OPT) --container-tool docker
	@echo "catalog-build: built $(CATALOG_IMG). Validate locally with 'make olm-validate' (kind+OLM); never auto-published."

.PHONY: catalog-push
catalog-push: ## PUBLISH GUARD — pushing the catalog image requires explicit PUSH=true and human approval.
	@if [ "$(PUSH)" != "true" ]; then \
	  echo "REFUSING to push: set PUSH=true AND obtain human approval (arch: publishing is human-gated)."; exit 1; \
	fi
	@echo "PUSH=true requested for $(CATALOG_IMG). This is an OUTWARD action — a human operator must run it intentionally."
	docker push $(CATALOG_IMG)

.PHONY: olm-validate
olm-validate: ## LOCAL kind+OLM validation of the catalog as a CatalogSource (never publishes).
	./hack/olm-validate.sh $(CATALOG_IMG)

##@ Helm charts (lint/package only — publishing is chart-releaser on the helm repo, never here)

.PHONY: helm-lint
helm-lint: ## helm lint both source charts (read-only).
	$(HELM) lint $(CHARTS_DIR)/valkey-operator
	$(HELM) lint $(CHARTS_DIR)/valkey-db

.PHONY: helm-template
helm-template: ## Render both charts to stdout for inspection (read-only).
	$(HELM) template valkey-operator $(CHARTS_DIR)/valkey-operator
	$(HELM) template valkey-db $(CHARTS_DIR)/valkey-db

.PHONY: helm-package
helm-package: $(LOCALBIN) ## Package both charts into ./bin (local .tgz only; NO push — publishing is chart-releaser).
	$(HELM) package $(CHARTS_DIR)/valkey-operator -d $(LOCALBIN)
	$(HELM) package $(CHARTS_DIR)/valkey-db -d $(LOCALBIN)
	@echo "helm-package: wrote .tgz to $(LOCALBIN). Charts publish via chart-releaser in percona-helm-charts (Chart.yaml version bump), NOT from here."

.PHONY: helm-crds-sync
helm-crds-sync: ## Copy deploy/crd.yaml into the operator chart's crds/ (the manual CRD-sync step; arch 10 §3.3).
	cp deploy/crd.yaml $(CHARTS_DIR)/valkey-operator/crds/crd.yaml
	@echo "helm-crds-sync: $(CHARTS_DIR)/valkey-operator/crds/crd.yaml <= deploy/crd.yaml"
