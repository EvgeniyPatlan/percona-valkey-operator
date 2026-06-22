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

# Embed the operator version into the binary at build time.
VERSION_LDFLAGS     ?= -X $(MODULE)/pkg/version.gitVersion=$(VERSION)

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

CONTROLLER_GEN_VERSION ?= v0.19.0
KUSTOMIZE_VERSION      ?= v5.7.1
ENVTEST_VERSION        ?= release-0.23
MOCKGEN_VERSION        ?= v0.6.0
GOLANGCI_LINT_VERSION  ?= v2.12.2
# Declared-but-deferred (OLM, wired in M7):
OPERATOR_SDK_VERSION   ?= v1.41.1
OPM_VERSION            ?= v1.55.0

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

##@ Release (NOT wired until M7 — guarded no-ops so the vocabulary is complete)

.PHONY: release after-release update-version bundle bundle-build bundle-push catalog-build catalog-push
release after-release update-version bundle bundle-build bundle-push catalog-build catalog-push:
	@echo "[$@] not wired until M7 (Distribution). Pass VERSION=x.y.z when implemented."; exit 1
